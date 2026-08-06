package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/objenc"
)

// errRetrofitNotFound stands in for kubectl's "NotFound" exit.
var errRetrofitNotFound = errors.New("Error from server (NotFound)")

// harborPodsJSON builds a `kubectl get pods -o json` payload for one harbor-registry
// pod, with the obj-proxy CA mount either present on both S3-touching containers or
// absent entirely — the two states the retrofit has to tell apart.
func harborPodsJSON(t *testing.T, withCA bool) string {
	t.Helper()
	container := func(name string) map[string]any {
		c := map[string]any{"name": name}
		if withCA {
			c["env"] = []map[string]any{{"name": objenc.SsecCertDirEnv, "value": "/etc/ssl/certs:" + objenc.ObjProxyCAMount}}
			c["volumeMounts"] = []map[string]any{{"name": objenc.ObjProxyCAVolume, "mountPath": objenc.ObjProxyCAMount}}
		}
		return c
	}
	pod := map[string]any{
		"metadata": map[string]any{"name": objenc.HarborRegistryLabel + "-7d9f-abcde"},
		"spec": map[string]any{
			"containers": []map[string]any{container("registry"), container("registryctl")},
		},
	}
	if withCA {
		pod["spec"].(map[string]any)["volumes"] = []map[string]any{{"name": objenc.ObjProxyCAVolume}}
	}
	raw, err := json.Marshal(map[string]any{"items": []any{pod}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// retrofitHarness swaps in the read/write seams and records the kubectl verbs the
// retrofit issues.
type retrofitHarness struct {
	calls    []string
	podsCA   []bool // successive answers to "do the running pods carry the CA?"
	policy   bool
	rollFail bool
}

func (h *retrofitHarness) install(t *testing.T) {
	t.Helper()
	origRead, origWrite, origRolled, origBudget := objencDeps, harborCARetrofitKubectl, harborCARetrofitRolledOut, harborWaitBudget
	t.Cleanup(func() {
		objencDeps, harborCARetrofitKubectl, harborCARetrofitRolledOut, harborWaitBudget = origRead, origWrite, origRolled, origBudget
	})
	harborWaitBudget = 50 * time.Millisecond

	reads := 0
	readPods := func(args ...string) (string, error) {
		h.calls = append(h.calls, strings.Join(args, " "))
		i := reads
		reads++
		if i >= len(h.podsCA) {
			i = len(h.podsCA) - 1
		}
		return harborPodsJSON(t, h.podsCA[i]), nil
	}
	// Swap the whole capability set rather than one package-level var: the pod
	// read the retrofit drives is objenc's, and objenc takes it as a Deps field.
	base := origRead()
	base.KubectlOut = readPods
	objencDeps = func() objenc.Deps { return base }

	harborCARetrofitKubectl = func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		h.calls = append(h.calls, joined)
		if strings.Contains(joined, "clusterpolicy") && !h.policy {
			return "", errRetrofitNotFound
		}
		if strings.Contains(joined, "rollout restart") && h.rollFail {
			return "", errRetrofitNotFound
		}
		return "", nil
	}
	harborCARetrofitRolledOut = func(string, string) bool { return !h.rollFail }
}

func (h *retrofitHarness) did(substr string) bool {
	for _, c := range h.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// The retrofit exists for exactly one situation: harbor-registry pods that apl-core
// started BEFORE the Kyverno policy existed. Admission-time mutation cannot reach a
// running pod, so nothing else in the component fixes them — and once the CoreDNS
// rewrite is live they cannot complete a single S3 call. They do not crash, so
// nothing restarts them and nothing reports it.
func TestHarborCARetrofitRollsPodsThatPredateThePolicy(t *testing.T) {
	h := &retrofitHarness{policy: true, podsCA: []bool{false, true}}
	h.install(t)

	retrofitHarborObjProxyCA()

	if !h.did("rollout restart deploy/harbor-registry") {
		t.Errorf("pods were missing the CA and the retrofit did not roll them; calls: %v", h.calls)
	}
}

// Restarting harbor-registry is a brief interruption to every image push and pull.
// Paying it on every bootstrap — when the pods were already admitted correctly —
// would make the gate itself the outage it is meant to prevent.
func TestHarborCARetrofitDoesNothingWhenPodsAlreadyTrustTheCA(t *testing.T) {
	h := &retrofitHarness{policy: true, podsCA: []bool{true}}
	h.install(t)

	retrofitHarborObjProxyCA()

	if h.did("rollout restart") {
		t.Errorf("pods already carried the CA but the retrofit rolled them anyway — that is a needless "+
			"registry outage on every run; calls: %v", h.calls)
	}
}

// objProxy is default-disabled. On a cluster without it there is no policy, no CA,
// and no reason for this to touch Harbor at all — least of all to read every pod in
// the namespace and conclude they are all "missing" a mount nothing ever adds.
func TestHarborCARetrofitIsInertWithoutTheComponent(t *testing.T) {
	h := &retrofitHarness{policy: false, podsCA: []bool{false}}
	h.install(t)

	retrofitHarborObjProxyCA()

	if h.did("rollout restart") || h.did("get pods") {
		t.Errorf("the objProxy ClusterPolicy is absent, so the component is not enabled here, yet the "+
			"retrofit still went looking at Harbor; calls: %v", h.calls)
	}
}

// The restart is only a fix if the replacement pods actually came back mutated. If
// Kyverno was down they did not, and reporting success there is the difference
// between a warning an operator can act on and a silent Harbor outage.
func TestHarborCARetrofitReportsWhenTheRestartDidNotTake(t *testing.T) {
	h := &retrofitHarness{policy: true, podsCA: []bool{false, false}}
	h.install(t)

	out := captureStderr(t, retrofitHarborObjProxyCA)

	if !strings.Contains(out, "::warning::") {
		t.Errorf("pods still lacked the CA after the roll and nothing was reported; stderr: %q", out)
	}
}
