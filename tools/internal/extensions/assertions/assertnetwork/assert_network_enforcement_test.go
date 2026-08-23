package assertnetwork

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// ── net-probe ────────────────────────────────────────────────────────────────

func TestClassifyDialError(t *testing.T) {
	if got := classifyDialError(nil); got != "connected" {
		t.Errorf("nil must be connected, got %q", got)
	}
	if got := classifyDialError(&net.DNSError{Err: "no such host"}); got != "dns" {
		t.Errorf("a DNS failure must be distinguishable, got %q", got)
	}
	if got := classifyDialError(errors.New("dial tcp 10.0.0.1:80: connect: connection refused")); got != "refused" {
		t.Errorf("expected refused, got %q", got)
	}
	// A sidecar refusing plaintext RESETS; a NetworkPolicy drop usually times out.
	// Reporting which is the difference between checking Istio and checking Cilium.
	if got := classifyDialError(errors.New("read tcp: connection reset by peer")); got != "refused" {
		t.Errorf("a reset must classify as refused, got %q", got)
	}
	if got := classifyDialError(errors.New("dial tcp 10.0.0.1:80: i/o timeout")); got != "timeout" {
		t.Errorf("expected timeout, got %q", got)
	}
	if got := classifyDialError(errors.New("something else")); !strings.HasPrefix(got, "error:") {
		t.Errorf("an unrecognised error must carry its text, got %q", got)
	}
}

func TestProbeTCP(t *testing.T) {
	orig := dialTCP
	t.Cleanup(func() { dialTCP = orig })

	dialTCP = func(string, time.Duration) error { return nil }
	if r := probeTCP("h:80", time.Second); !r.Connected || r.Reason != "connected" {
		t.Errorf("unexpected %+v", r)
	}
	dialTCP = func(string, time.Duration) error { return errors.New("connect: connection refused") }
	if r := probeTCP("h:80", time.Second); r.Connected || r.Reason != "refused" {
		t.Errorf("unexpected %+v", r)
	}
}

// ── the verdict logic ────────────────────────────────────────────────────────

func TestEvalEnforcementProbe(t *testing.T) {
	connected := func(t string) netProbeResult { return netProbeResult{Target: t, Connected: true, Reason: "connected"} }
	blocked := func(t string) netProbeResult { return netProbeResult{Target: t, Connected: false, Reason: "timeout"} }

	// Control connected, denied blocked → enforcement observed.
	if v := evalEnforcementProbe("netpol", connected("api:443"), blocked("loki:80"), "because"); v.FailWhy != "" {
		t.Errorf("control up + denied blocked must pass: %s", v.FailWhy)
	}

	// Control connected, denied ALSO connected → the real finding.
	v := evalEnforcementProbe("netpol", connected("api:443"), connected("loki:80"), "the CNI is not enforcing")
	if v.FailWhy == "" {
		t.Fatal("a denied target that connects must fail")
	}
	if !strings.Contains(v.FailWhy, "NOT ENFORCED") {
		t.Errorf("the failure must say it is not enforced, got %q", v.FailWhy)
	}
	if v.Inconclu {
		t.Error("a clean negative result is conclusive, not inconclusive")
	}
}

// THE arm that matters. If the positive control could not connect, the pod's
// networking is broken and "the denied target was blocked" is evidence of
// nothing. A gate that reported this as a pass would certify a policy it never
// exercised — which is exactly how a negative test silently becomes decorative.
func TestEvalEnforcementProbeInconclusiveWhenControlFails(t *testing.T) {
	deadControl := netProbeResult{Target: "api:443", Connected: false, Reason: "timeout"}
	blockedDenied := netProbeResult{Target: "loki:80", Connected: false, Reason: "timeout"}

	v := evalEnforcementProbe("netpol", deadControl, blockedDenied, "because")
	if v.FailWhy == "" {
		t.Fatal("a failed positive control must FAIL — both dials failing is not proof of enforcement")
	}
	if !v.Inconclu {
		t.Error("it must be marked inconclusive, not reported as a policy finding")
	}
	if !strings.Contains(v.FailWhy, "INCONCLUSIVE") || !strings.Contains(v.FailWhy, "did NOT observe enforcement") {
		t.Errorf("the message must say plainly that nothing was observed, got %q", v.FailWhy)
	}

	// And it stays inconclusive even when the denied target CONNECTED — with a
	// broken control we cannot tell that apart from a half-working network.
	if v := evalEnforcementProbe("netpol", deadControl, netProbeResult{Target: "loki:80", Connected: true}, "x"); !v.Inconclu {
		t.Error("a failed control must dominate the verdict regardless of the denied dial")
	}
}

func TestResultFromExit(t *testing.T) {
	if r := resultFromExit("t", 0, true, ""); !r.Connected {
		t.Error("exit 0 is connected")
	}
	if r := resultFromExit("t", 1, true, ""); r.Connected || r.Reason != "blocked" {
		t.Errorf("exit 1 is blocked, got %+v", r)
	}
	// Exit 2 means the probe could not run — NOT that the target was blocked.
	// Folding it into "blocked" would turn a broken probe into a passing gate.
	if r := resultFromExit("t", 2, true, ""); r.Connected || !strings.Contains(r.Reason, "could not run") {
		t.Errorf("exit 2 must be distinguishable from blocked, got %+v", r)
	}
	if r := resultFromExit("t", 0, false, ""); r.Connected {
		t.Error("a missing exit code must not read as connected")
	}
}

func TestContainerExit(t *testing.T) {
	raw := []byte(`{"status":{"containerStatuses":[
	  {"name":"control","state":{"terminated":{"exitCode":0}}},
	  {"name":"denied","state":{"terminated":{"exitCode":1}}}
	]}}`)
	got, err := containerExit(raw)
	if err != nil || got["control"] != 0 || got["denied"] != 1 {
		t.Fatalf("unexpected (%v,%v)", got, err)
	}
	// A container still running means the pod did not finish — reading its absent
	// exit code as anything would be a guess.
	running := []byte(`{"status":{"containerStatuses":[{"name":"control","state":{"running":{}}}]}}`)
	if _, err := containerExit(running); err == nil {
		t.Error("an unterminated container must be an error")
	}
	if _, err := containerExit([]byte(`{"status":{}}`)); err == nil {
		t.Error("no container statuses must be an error, not an empty pass")
	}
	if _, err := containerExit([]byte(`nope`)); err == nil {
		t.Error("an unparseable pod must be an error")
	}
}

// The probe pod MUST be outside the mesh. A meshed pod has its plaintext
// transparently upgraded to mTLS by its own sidecar, so the denied dial would
// SUCCEED — proving the mesh works while asserting the exact opposite.
func TestProbePodManifestStaysOutsideTheMesh(t *testing.T) {
	m := probePodManifest("ns", "img", "a:1", "d:2", "m:3", 5*time.Second)
	if !strings.Contains(m, `sidecar.istio.io/inject: "false"`) {
		t.Error("the probe pod must opt OUT of the mesh, or the mTLS negative asserts the opposite of what it claims")
	}
	// Separate containers, not initContainers: initContainers stop at the first
	// failure, and the denied dial is EXPECTED to fail — the control still has to run.
	if strings.Contains(m, "initContainers") {
		t.Error("the dials must be separate containers; initContainers would stop at the expected failure")
	}
	// The DNS allow is asserted by label VALUE in
	// TestProbePodManifestAllowsBothDNSLabels; pinning the exact matchLabels line
	// here would fail the moment it became a matchExpressions covering both
	// conventions, which is what LKE-Enterprise needs.
	for _, want := range []string{"a:1", "d:2", "m:3", "default-deny-egress", "k8s-app"} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest is missing %q", want)
		}
	}
	// DNS must be allowed, or every dial fails on resolution and the control
	// cannot distinguish "policy blocked it" from "the name never resolved".
	if !strings.Contains(m, "port: 53") {
		t.Error("the egress policy must allow DNS")
	}
}

func TestRunAssertNetworkEnforcementRejectsEmptyAndUnknownChecks(t *testing.T) {
	orig := resolveProbeImage
	t.Cleanup(func() { resolveProbeImage = orig })
	resolveProbeImage = func() (string, error) { return "img", nil }

	if err := runCIAssertNetworkEnforcement(netEnforceOpts{}); err == nil {
		t.Error("no checks must fail rather than pass having verified nothing")
	}
}

// The scratch namespace must be deleted even when the run fails — this gate runs
// on a cluster ten other lanes are still asserting against.
func TestRunAssertNetworkEnforcementAlwaysCleansUp(t *testing.T) {
	oImg, oApply, oDel := resolveProbeImage, applyProbeManifest, deleteProbeNamespace
	t.Cleanup(func() { resolveProbeImage, applyProbeManifest, deleteProbeNamespace = oImg, oApply, oDel })

	resolveProbeImage = func() (string, error) { return "img", nil }
	applyProbeManifest = func(string) (string, error) { return "", errors.New("apply refused") }
	var deletes int
	deleteProbeNamespace = func(string) { deletes++ }

	if err := runCIAssertNetworkEnforcement(netEnforceOpts{
		checks: []string{"netpol"}, namespace: "ns", timeout: time.Millisecond,
	}); err == nil {
		t.Fatal("a failed apply must fail the gate")
	}
	// One pre-emptive delete (in case a previous run leaked) plus the deferred one.
	if deletes < 2 {
		t.Errorf("the scratch namespace must be cleaned up on the failure path, got %d deletes", deletes)
	}
}

// The probe already classifies every dial as refused / timeout / dns; the gate
// used to collapse all of them into "blocked" and then tell the operator to go
// check DNS by hand. A failed positive control is the case where that evidence
// matters most, so it must reach the verdict text.
func TestResultFromExitCarriesTheProbesOwnReason(t *testing.T) {
	r := resultFromExit("kubernetes.default.svc.cluster.local:443", 1, true, "dns: lookup failed")
	if r.Connected {
		t.Fatal("exit 1 must remain blocked")
	}
	if !strings.Contains(r.Reason, "dns: lookup failed") {
		t.Errorf("reason = %q — the probe's own classification must survive into the verdict, or the "+
			"gate discards the only evidence it collected", r.Reason)
	}
}

// An unreadable log must not change the verdict — only how well it is explained.
func TestResultFromExitWithoutALogIsUnchanged(t *testing.T) {
	if r := resultFromExit("t", 1, true, ""); r.Reason != "blocked" {
		t.Errorf("reason = %q, want the bare verdict when no log could be read", r.Reason)
	}
}

// The probe's DNS allow must match CoreDNS on LKE-Enterprise, where it is
// labelled k8s-app=coredns and there is no kube-dns Service at all. Selecting
// only kube-dns matches nothing there, so DNS egress is denied and every dial —
// including the positive control — fails to resolve.
func TestProbePodManifestAllowsBothDNSLabels(t *testing.T) {
	m := probePodManifest("ns", "img", "a:1", "d:2", "m:3", time.Second)
	for _, want := range []string{"kube-dns", "coredns"} {
		if !strings.Contains(m, want) {
			t.Errorf("the DNS egress allow does not mention %q — a cluster labelling CoreDNS that way "+
				"gets no DNS, and every probe fails to resolve rather than reporting enforcement", want)
		}
	}
	if !strings.Contains(m, "matchExpressions") {
		t.Error("expected a matchExpressions/In selector so one rule covers both label conventions")
	}
}

// The mtls check must not accept a TIMEOUT as proof the mesh refused anything.
// A timeout is a packet drop — the CNI discarded it and Istio never saw the
// connection — so counting it would make this check a second copy of the netpol
// check, passing on a property the run never exercised.
func TestEvalEnforcementProbeMTLSRejectsATimeoutAsProof(t *testing.T) {
	ctrl := netProbeResult{Target: "ctrl", Connected: true, Reason: "connected"}
	dropped := netProbeResult{Target: "harbor-core...:80", Reason: "blocked: probe harbor-core...:80: timeout"}

	v := evalEnforcementProbe("mtls", ctrl, dropped, "why")
	if v.FailWhy == "" || !v.Inconclu {
		t.Error("a timeout on the mtls dial must be INCONCLUSIVE — Istio was never consulted")
	}

	// A reset IS the mesh refusing, and must pass.
	refused := netProbeResult{Target: "harbor-core...:80", Reason: "blocked: probe harbor-core...:80: refused"}
	if v := evalEnforcementProbe("mtls", ctrl, refused, "why"); v.FailWhy != "" {
		t.Errorf("a refused plaintext dial is exactly what the mesh does: %s", v.FailWhy)
	}
}

// The netpol check is deliberately NOT held to that rule: a drop is precisely
// what it asserts.
func TestEvalEnforcementProbeNetpolAcceptsATimeout(t *testing.T) {
	ctrl := netProbeResult{Target: "ctrl", Connected: true, Reason: "connected"}
	dropped := netProbeResult{Target: "loki...:80", Reason: "blocked: probe loki...:80: timeout"}
	if v := evalEnforcementProbe("netpol", ctrl, dropped, "why"); v.FailWhy != "" {
		t.Errorf("a CNI drop is what the netpol check is for: %s", v.FailWhy)
	}
}

// The probe policy must OPEN the path to the mtls target's namespace. If the
// NetworkPolicy also denies it, the CNI drops the packet first and the mesh is
// never consulted — which is the confound the check above now refuses.
func TestProbePodManifestAllowsTheMTLSTargetNamespace(t *testing.T) {
	m := probePodManifest("ns", "img", "a:1", "loki-gateway.monitoring.svc.cluster.local:80",
		"harbor-core.harbor.svc.cluster.local:80", time.Second)
	if !strings.Contains(m, "kubernetes.io/metadata.name: harbor") {
		t.Error("egress to the mtls target's namespace must be allowed, or the CNI blocks it before Istio can")
	}
	// The NETPOL denied target must stay unallowed — that one is about the CNI.
	if strings.Contains(m, "kubernetes.io/metadata.name: monitoring") {
		t.Error("the netpol denied target's namespace must NOT be allowed; the check asserts the CNI blocks it")
	}
}

func TestServiceNamespaceOf(t *testing.T) {
	for in, want := range map[string]string{
		"harbor-core.harbor.svc.cluster.local:80":      "harbor",
		"loki-gateway.monitoring.svc.cluster.local:80": "monitoring",
		"kubernetes.default.svc.cluster.local:443":     "default",
		"nodots:80": "",
	} {
		if got := serviceNamespaceOf(in); got != want {
			t.Errorf("serviceNamespaceOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// A PERMISSIVE namespace ACCEPTS plaintext by design, so the dial succeeding
// there is correct behaviour. harbor ships all three of its PeerAuthentication
// documents PERMISSIVE on purpose — ADR 0010 step 3 — and asserting STRICT
// before that flip reds every cluster for a rollout step nobody has taken.
func TestMeshEnforcesSTRICT(t *testing.T) {
	if meshEnforcesSTRICT([]string{"PERMISSIVE", "PERMISSIVE", "PERMISSIVE"}) {
		t.Error("an all-PERMISSIVE namespace is not enforcing")
	}
	if meshEnforcesSTRICT(nil) {
		t.Error("no PeerAuthentication at all means the mesh default applies, which is PERMISSIVE here")
	}
	if !meshEnforcesSTRICT([]string{"PERMISSIVE", "STRICT"}) {
		t.Error("a STRICT mode anywhere in the namespace means it is enforcing")
	}
	if !meshEnforcesSTRICT([]string{"strict"}) {
		t.Error("the mode comparison must not be case-sensitive")
	}
}
