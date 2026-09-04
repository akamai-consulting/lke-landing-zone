package assertobs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// TestMain zeroes the probe retry delay — without it every stubbed-error test
// pays 6 real seconds (3 retries x 3s). internal/converge lost this line in
// extraction and its suite went 4s -> 568s.
func TestMain(m *testing.M) {
	kubectlprobe.Delay = 0
	os.Exit(m.Run())
}

// withExecOutput swaps this package's Exec seam AND kubectlprobe's. Both, because
// these lanes read the cluster two ways; stubbing one leaves the other reaching
// for a real cluster, which earlier extractions established is a hang rather than
// a failure.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	origExec, origProbe := caps.Exec, kubectlprobe.Exec
	caps.Exec = fn
	kubectlprobe.Exec = fn
	t.Cleanup(func() { caps.Exec, kubectlprobe.Exec = origExec, origProbe })
}

func captureStdout(t *testing.T, fn func()) string { t.Helper(); return capture(t, &os.Stdout, fn) }
func captureStderr(t *testing.T, fn func()) string { t.Helper(); return capture(t, &os.Stderr, fn) }

func capture(t *testing.T, target **os.File, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := *target
	*target = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	*target = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// withKubectl stubs the Exec seam to answer kubectl invocations via a handler
// keyed on the joined args; non-kubectl shell-outs error.
// It stubs THREE seams, not one: Exec, kubectlprobe's, and KubectlOut. This
// package reads the cluster three ways and the readiness lanes use the
// string-returning one — stubbing only Exec left deploymentRolledOut reading a
// real cluster, which showed up as "a clean 2/2 read must count as rolled out".
// walPVCClassArgs is the jsonpath read the durability check makes for the WAL
// PVC's StorageClass. Named here because every assert-loki fixture has to answer
// it: the check fails closed on an unreadable PVC, so a stub that does not
// recognise this call turns every happy-path fixture red for the wrong reason.
const walPVCClassArgs = "storageClassName"

// answerWALPVCClass wraps a fixture handler so the PVC lookup resolves to the
// class the overlay asserts, leaving the fixture to describe only what it is
// actually about.
func answerWALPVCClass(h func(args string) ([]byte, error)) func(string) ([]byte, error) {
	return func(args string) ([]byte, error) {
		if strings.Contains(args, walPVCClassArgs) {
			return []byte(healthyLokiWALClass), nil
		}
		return h(args)
	}
}

// lokiCeilingArgs is the shape of the ceiling read, matched the same way and for
// the same reason as walPVCClassArgs: it is infrastructure every Loki fixture
// needs answered, not something any individual fixture is about.
const lokiCeilingArgs = "data.config"

// healthyLokiConfig is the rendered config once the overlay's replay ceiling has
// applied — the state assert-loki should call healthy.
const healthyLokiConfig = "ingester:\n  chunk_encoding: snappy\n  wal:\n    replay_memory_ceiling: 1536MB\n"

// answerLokiReplayCeiling wraps a fixture handler so the ConfigMap read resolves
// to a config carrying the asserted ceiling.
func answerLokiReplayCeiling(h func(args string) ([]byte, error)) func(string) ([]byte, error) {
	return func(args string) ([]byte, error) {
		if strings.Contains(args, lokiCeilingArgs) {
			return []byte(healthyLokiConfig), nil
		}
		return h(args)
	}
}

func withKubectl(t *testing.T, h func(args string) ([]byte, error)) {
	t.Helper()
	h = answerWALPVCClass(answerLokiReplayCeiling(h))
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			return nil, fmt.Errorf("unexpected command %q", name)
		}
		return h(strings.Join(args, " "))
	})
	origOut := caps.KubectlOut
	caps.KubectlOut = func(args ...string) (string, error) {
		b, err := h(strings.Join(args, " "))
		return string(b), err
	}
	t.Cleanup(func() { caps.KubectlOut = origOut })
}

// items wraps item JSON blobs into a kubectl list response.
func items(blobs ...string) []byte {
	return []byte(`{"items":[` + strings.Join(blobs, ",") + `]}`)
}

// errRetrofitNotFound stands in for kubectl's "NotFound" exit — a local copy of
// package main's sentinel, exactly as internal/objenc's errNotFound is. A one-line
// sentinel does not justify an export, and a test fixture cannot cross a package
// boundary.
var errRetrofitNotFound = errors.New("Error from server (NotFound)")

// reconcilerRuleCRD is the PrometheusRule under test, repo-relative.
//
// THE PATH CHANGED WITH THE PACKAGE. It was "../../../../../platform-apl/..." from
// cmd/llz and is the same depth from internal/assertobs, which is luck rather than
// design — a relative path in a test is a dependency on where the test file sits,
// and moving the file silently re-points it. Checked rather than assumed.
const reconcilerRuleCRD = "../../../../../platform-apl/components/llzReconciler/llz-reconciler/prometheusrule.yaml"

// containsString: the definition travelled out of package main with a file this
// extraction moved, leaving both sides using it. Defined here rather than hunted
// for — it is three lines and slices.Contains-shaped.
func containsString(hay []string, want string) bool {
	for _, h := range hay {
		if h == want {
			return true
		}
	}
	return false
}

// healthyLokiIngesterPod is the pod every assert-loki happy-path fixture returns.
//
// IT IS AN INGESTER, not a generic `loki-0`, because the lane now asks a question
// only an ingester can answer: does it have the WAL-replay headroom and the PVC
// that keep it out of a self-perpetuating OOM crashloop. The old fixture — one
// container named `loki`, no volumes — described the SINGLE-BINARY topology LLZ
// stopped running, so a fixture built on it would have proved the check passes on
// a shape no cluster has.
// healthyLokiWALClass is the StorageClass the overlay asserts for the WAL PVC.
const healthyLokiWALClass = "block-storage-retain"

const healthyLokiIngesterPod = `{"metadata":{"namespace":"monitoring","name":"loki-ingester-0"},
	"spec":{"containers":[{"name":"ingester","resources":{"limits":{"memory":"3Gi"}}}],
	 "volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"data-loki-ingester-0"}},
	  {"name":"config","configMap":{"name":"loki"}}]},
	"status":{"phase":"Running","containerStatuses":[{"name":"ingester","ready":true}]}}`
