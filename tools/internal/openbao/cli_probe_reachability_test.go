package openbao

import (
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/baoread"
	"gopkg.in/yaml.v3"
)

// openbaoChartValues is the path to the llz-openbao-platform values, relative to
// the repo root.
const openbaoChartValues = "kubernetes-charts/llz-openbao-platform/values.yaml"

// THE CONSTRAINT THIS GUARDS
//
// ADR 0010 left OpenBao with two listeners and NO port a kubelet probe can
// legally reach over the network:
//
//	[::]:8200        pod network — mTLS, client certificate REQUIRED
//	127.0.0.1:8210   loopback    — TLS, no client certificate
//
// A kubelet httpGet probe holds no client identity, so :8200 rejects it with
// `remote error: tls: certificate required`. And httpGet has no `host` field in
// the upstream subchart's template (charts/openbao server-statefulset.yaml
// renders path/port/scheme only), so kubelet dials the POD IP — where nothing is
// listening on 8210, because that listener binds loopback only. Both spellings
// end in the same place: the liveness probe fails forever and kubelet SIGKILLs
// the container on a loop. A live e2e cluster reached 18 restarts this way, and
// the first attempted repair (moving the httpGet from 8200 to 8210) would have
// reproduced it with a different error string.
//
// Therefore BOTH probes must be `exec`, which runs inside the pod's own network
// namespace — where loopback is reachable and, unlike kubelet, the process CAN
// present a client certificate (see TestOpenBaoInPodClientsCarryAClientCertificate).
// This is a property of the deployment topology, not a style choice, and it is
// invisible in a diff — the probes live in the upstream subchart's values, so
// nothing that looks like a pod spec changes when it breaks. Hence a test.
func TestOpenBaoProbesAreExecNotHTTPGet(t *testing.T) {
	server := openbaoServerValues(t)

	// The subchart selects httpGet for readiness by the PRESENCE of `path`
	// (`{{- if .Values.server.readinessProbe.path }}`), falling back to its
	// built-in `bao status` exec. So readiness is correct exactly when `path` is
	// absent — setting it, even pointed at 8210, is the bug.
	if readiness, ok := server["readinessProbe"].(map[string]any); ok {
		if p, present := readiness["path"]; present {
			t.Errorf("server.readinessProbe.path is set (%v) — that switches the subchart to an httpGet probe.\n"+
				"kubelet dials the POD IP (httpGet has no host field), but the cert-free listener binds 127.0.0.1 only,\n"+
				"and the pod-network listener requires a client certificate kubelet cannot present. Remove `path` to\n"+
				"keep the subchart's `bao status` exec, which runs inside the pod.", p)
		}
	}

	// Liveness is the mirror image: the subchart prefers exec only when
	// `execCommand` is non-empty (`{{- if .Values.server.livenessProbe.execCommand }}`),
	// otherwise it renders httpGet from path/port.
	liveness, ok := server["livenessProbe"].(map[string]any)
	if !ok {
		t.Fatal("server.livenessProbe missing from " + openbaoChartValues)
	}
	cmd, _ := liveness["execCommand"].([]any)
	if len(cmd) == 0 {
		t.Error("server.livenessProbe.execCommand is empty — the subchart falls back to an httpGet probe,\n" +
			"which kubelet sends to the pod IP. Neither listener answers there without a client certificate,\n" +
			"so kubelet SIGKILLs the container on a loop (observed live: 18 restarts).")
	}

	// A liveness exec that cannot survive the bootstrap window is its own outage:
	// OpenBao comes up sealed and stays sealed until `bao operator init` runs, and
	// `bao status` exits 2 while sealed. If the command does not treat 2 as alive,
	// kubelet kills the pod before bootstrap can ever reach it.
	joined := ""
	for _, a := range cmd {
		joined += " " + toString(a)
	}
	if strings.Contains(joined, "bao status") && !strings.Contains(joined, "-eq 2") {
		t.Error("server.livenessProbe.execCommand runs `bao status` but never accepts exit code 2.\n" +
			"2 means sealed/uninitialized, which is the NORMAL state from container start until\n" +
			"bootstrap-openbao runs `bao operator init` — treating it as failure restarts the pod\n" +
			"out from under the bootstrap.")
	}
}

// subchartServerEnvNames are the env names the upstream openbao subchart writes
// into the server container itself (charts/openbao server-statefulset.yaml).
// Redeclaring any of them from extraEnvironmentVars is the bug below.
var subchartServerEnvNames = []string{
	"HOST_IP", "POD_IP", "BAO_K8S_POD_NAME", "BAO_K8S_NAMESPACE",
	"BAO_ADDR", "BAO_API_ADDR", "SKIP_CHOWN", "SKIP_SETCAP", "HOSTNAME",
	"BAO_CLUSTER_ADDR", "BAO_RAFT_NODE_ID", "HOME",
}

// THE BUG THIS GUARD EXISTS FOR — a real, whole-wasted-e2e-run failure.
//
// The subchart hardcodes BAO_ADDR=https://127.0.0.1:8200 with no knob to change
// it. The obvious move is to override it from extraEnvironmentVars, which the
// chart renders AFTER the subchart's own env — so "the later duplicate wins".
//
// It does not win. `container.env` is a server-side-apply list-map keyed on
// `name`, and SSA REJECTS duplicate keys outright rather than taking the last.
// The openbao Application syncs with ServerSideApply=true, so the apply failed,
// the StatefulSet was never created, and the app sat OutOfSync/Healthy with no
// workload — no pod, no restart, no container log, nothing that points at an env
// var. `helm template`, `helm lint` and a plain `kubectl apply` all accept the
// duplicate happily, so nothing local catches it either.
//
// Hence: never redeclare a name the subchart already sets. To change how an
// in-pod client behaves, add NEW names (BAO_CLIENT_CERT/BAO_CLIENT_KEY/BAO_CACERT
// are the intended lever) rather than fighting an existing one.
func TestOpenBaoExtraEnvNeverShadowsSubchartEnv(t *testing.T) {
	server := openbaoServerValues(t)
	extra, _ := server["extraEnvironmentVars"].(map[string]any)
	for name := range extra {
		for _, reserved := range subchartServerEnvNames {
			if strings.EqualFold(name, reserved) {
				t.Errorf("server.extraEnvironmentVars redeclares %q, which the upstream subchart already\n"+
					"sets on the server container. That renders TWO env entries with the same name, and\n"+
					"`container.env` is a server-side-apply list-map keyed on name — the apiserver REJECTS\n"+
					"duplicate keys, so the StatefulSet is never applied and the Argo app sits OutOfSync\n"+
					"with no pod at all. Add a new env name instead of overriding this one.", name)
			}
		}
	}
}

// The in-pod probes reach :8200, which ADR 0010 made client-certificate-only, so
// they must carry an identity. That identity has to be one the pod ALREADY
// mounts: a fresh Secret would become a non-optional volume the container blocks
// on at startup, trading a probe failure for a scheduling failure.
func TestOpenBaoInPodClientsCarryAClientCertificate(t *testing.T) {
	server := openbaoServerValues(t)
	extra, ok := server["extraEnvironmentVars"].(map[string]any)
	if !ok {
		t.Fatal("server.extraEnvironmentVars is missing — the in-pod `bao` clients (both probes)\n" +
			"then reach the mTLS listener with no client certificate and fail the handshake.")
	}
	for _, k := range []string{"BAO_CLIENT_CERT", "BAO_CLIENT_KEY"} {
		v := toString(extra[k])
		if v == "" {
			t.Errorf("server.extraEnvironmentVars.%s is unset — the readiness exec (`bao status`,\n"+
				"hardcoded by the subchart to the container's BAO_ADDR of :8200) presents no client\n"+
				"certificate and is rejected with `remote error: tls: certificate required`.", k)
			continue
		}
		// The path must actually be mounted, or the env is a promise the pod cannot keep.
		if !strings.Contains(rawOpenBaoValues(t), "mountPath: "+path.Dir(v)) {
			t.Errorf("server.extraEnvironmentVars.%s = %q, but nothing mounts %q in server.volumeMounts —\n"+
				"the probe would read a nonexistent file.", k, v, path.Dir(v))
		}
	}
	// llz's exec paths do NOT go through the PodSpec, so they may (and must) point
	// themselves at the cert-free loopback listener explicitly.
	if !sliceContains(baoread.LoopbackEnv(), "BAO_ADDR="+baoread.LoopbackAddr) {
		t.Errorf("baoread.LoopbackEnv() does not export BAO_ADDR=%s: %v\n"+
			"OpenBao prefers a present BAO_* over its VAULT_* alias unconditionally (api/env.go), so a\n"+
			"VAULT_ADDR-only argv is silently overridden by the container's BAO_ADDR of :8200.",
			baoread.LoopbackAddr, baoread.LoopbackEnv())
	}
}

// The two assertions above are only meaningful if the loopback listener really is
// loopback-only — if it ever gains a pod-network bind, an httpGet probe becomes
// legal again and these guards would be over-strict rather than load-bearing.
// Pin the premise so the guards get revisited deliberately, not silently.
func TestOpenBaoLoopbackListenerIsLoopbackOnly(t *testing.T) {
	raw := rawOpenBaoValues(t)
	re := regexp.MustCompile(`address\s*=\s*"([^"]*:` + baoread.LoopbackPort + `)"`)
	m := re.FindStringSubmatch(raw)
	if m == nil {
		t.Fatalf("no listener bound to port %s found in %s — the loopback listener the probes and\n"+
			"llz depend on has moved or been removed.", baoread.LoopbackPort, openbaoChartValues)
	}
	if !strings.HasPrefix(m[1], "127.0.0.1:") {
		t.Errorf("listener on port %s binds %q, not 127.0.0.1.\n"+
			"If that is deliberate, revisit TestOpenBaoProbesAreExecNotHTTPGet: a pod-network bind makes\n"+
			"an httpGet probe reachable again — but it also re-exposes a certificate-free API port,\n"+
			"which is what ADR 0010 removed.", baoread.LoopbackPort, m[1])
	}
}

// rawOpenBaoValues returns kubernetes-charts/llz-openbao-platform/values.yaml as
// text, for the assertions that inspect the embedded HCL and volume mounts rather
// than the YAML structure.
func rawOpenBaoValues(t *testing.T) string {
	t.Helper()
	return readForTLSTest(t, repoRootForTLSTest(t), filepath.ToSlash(openbaoChartValues))
}

// openbaoServerValues loads kubernetes-charts/llz-openbao-platform/values.yaml and
// returns the `server` subtree the upstream chart is configured through.
func openbaoServerValues(t *testing.T) map[string]any {
	t.Helper()
	var doc struct {
		OpenBao struct {
			Server map[string]any `yaml:"server"`
		} `yaml:"openbao"`
	}
	raw := rawOpenBaoValues(t)
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("parse %s: %v", openbaoChartValues, err)
	}
	if doc.OpenBao.Server == nil {
		t.Fatalf("%s has no server block", openbaoChartValues)
	}
	return doc.OpenBao.Server
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}
