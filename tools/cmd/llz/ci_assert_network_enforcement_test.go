package main

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
	if r := resultFromExit("t", 0, true); !r.Connected {
		t.Error("exit 0 is connected")
	}
	if r := resultFromExit("t", 1, true); r.Connected || r.Reason != "blocked" {
		t.Errorf("exit 1 is blocked, got %+v", r)
	}
	// Exit 2 means the probe could not run — NOT that the target was blocked.
	// Folding it into "blocked" would turn a broken probe into a passing gate.
	if r := resultFromExit("t", 2, true); r.Connected || !strings.Contains(r.Reason, "could not run") {
		t.Errorf("exit 2 must be distinguishable from blocked, got %+v", r)
	}
	if r := resultFromExit("t", 0, false); r.Connected {
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
	for _, want := range []string{"a:1", "d:2", "m:3", "default-deny-egress", "k8s-app: kube-dns"} {
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
