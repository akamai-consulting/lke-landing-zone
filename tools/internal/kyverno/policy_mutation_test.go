package kyverno

import (
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
)

// recordingDeps is testDeps with the sleep seam recorded, so the poll CADENCE
// (not just the fact that it polls) is observable.
func recordingDeps(f *fakeKubectl, step time.Duration) (cigate.Deps, *[]time.Duration) {
	now, _ := fakeClock(step)
	var slept []time.Duration
	return cigate.Deps{
		Kubectl: f.run,
		Now:     now,
		Sleep:   func(d time.Duration) { slept = append(slept, d) },
	}, &slept
}

func onlySleep(t *testing.T, slept []time.Duration, want time.Duration, what string) {
	t.Helper()
	if len(slept) == 0 {
		t.Fatalf("%s: expected at least one poll sleep, got none — the loop is not backing off at all", what)
	}
	for i, d := range slept {
		if d != want {
			t.Fatalf("%s: sleep %d = %v, want %v", what, i, d, want)
		}
	}
}

// TestKyvernoReadinessPollsAtFiveSeconds pins the readiness poll interval. The
// deadline (WAIT_TIMEOUT_SECONDS, 900 by default) is only half the contract: a
// degenerate interval turns a 15-minute patient wait into a hot spin against the
// apiserver during cluster bootstrap, exactly when it is least able to absorb it.
func TestKyvernoReadinessPollsAtFiveSeconds(t *testing.T) {
	f := &fakeKubectl{responses: []kubectlRule{
		{match: "get crd clusterpolicies", out: "", ok: false}, // never ready
	}}
	// 30s budget with a 20s/read clock → the loop probes, sleeps once, then times out.
	d, slept := recordingDeps(f, 20*time.Second)
	o := Opts{
		policyManifest: "manifests/kyverno-pvc.yaml",
		fieldManager:   "fm",
		waitForKyverno: true,
		waitTimeout:    30 * time.Second,
	}
	if err := Apply(o, d); err != nil {
		t.Fatalf("readiness timeout must soft-fail, got %v", err)
	}
	onlySleep(t, *slept, 5*time.Second, "readiness poll")
}

// TestKyvernoRetrofitPollsAtFiveSeconds is the same contract for the retrofit
// wait, which runs against a ConfigMap another controller creates.
func TestKyvernoRetrofitPollsAtFiveSeconds(t *testing.T) {
	f := &fakeKubectl{responses: []kubectlRule{
		{match: "get configmap loki-gateway", out: "", ok: false}, // never appears
	}}
	d, slept := recordingDeps(f, 20*time.Second)
	o := Opts{
		policyManifest:    "manifests/kyverno-loki-gateway-resolver.yaml",
		fieldManager:      "fm",
		waitForKyverno:    false, // skip the readiness loop; only the retrofit poll sleeps
		retrofitConfigMap: "loki-gateway",
		retrofitNamespace: "monitoring",
		retrofitWait:      30 * time.Second,
	}
	if err := Apply(o, d); err != nil {
		t.Fatalf("retrofit is best-effort and must not error, got %v", err)
	}
	onlySleep(t, *slept, 5*time.Second, "retrofit poll")
}
