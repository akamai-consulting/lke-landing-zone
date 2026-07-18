package main

import (
	"strings"
	"testing"
	"time"
)

// aplTestDeps wires the shared fakeKubectl + fakeClock (ci_kyverno_test.go) into
// aplGateDeps with no real sleeping.
func aplTestDeps(f *fakeKubectl, step time.Duration) aplGateDeps {
	now, _ := fakeClock(step)
	return aplGateDeps{kubectl: f.run, now: now, sleep: func(time.Duration) {}}
}

func TestWaitAplPipelineAllReady(t *testing.T) {
	f := &fakeKubectl{} // every get + wait succeeds
	if err := waitAplPipeline(aplPipelineStages(), aplTestDeps(f, time.Second)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Every stage must produce an existence probe and a wait carrying ITS OWN
	// condTimeout, in order. Derived from the stage table rather than restating
	// the numbers: the budgets are sized from measurement and will be re-tuned, and
	// a test that hardcodes them fails on a re-size while proving nothing about the
	// behavior that matters (shape, ordering, per-stage timeout wiring).
	stages := aplPipelineStages()
	if len(stages) == 0 {
		t.Fatal("no stages — the gate would pass having waited for nothing")
	}
	for _, st := range stages {
		if !f.called("get " + st.resource) {
			t.Errorf("stage %q: no existence probe for %s\ncalls: %v", st.desc, st.resource, f.calls)
		}
		// A bare condition becomes --for=condition=X; a full clause is passed verbatim.
		forFlag := "--for=" + st.forClause
		if !strings.Contains(st.forClause, "=") {
			forFlag = "--for=condition=" + st.forClause
		}
		want := "wait " + forFlag + " " + st.resource + " --timeout=" + st.condTimeout
		if !f.called(want) {
			t.Errorf("stage %q: expected kubectl call containing %q\ncalls: %v", st.desc, want, f.calls)
		}
	}
	// Ordering: cert-manager (last) must come after Argo CD (first).
	if idx(f.calls, "crd/applications.argoproj.io") > idx(f.calls, "crd/certificates.cert-manager.io") {
		t.Errorf("stages ran out of order: %v", f.calls)
	}
}

// idx returns the position of the first call containing sub, or -1.
func idx(calls []string, sub string) int {
	for i, c := range calls {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

// TestAplPipelineBudgetsFitTheJob pins the invariant the old budgets broke: the
// gate's worst case must be reachable INSIDE the bootstrap job, or a genuinely
// slow run is killed by GitHub with no verdict instead of failing with the
// message this gate exists to print. The job is timeout-minutes: 70 on the
// bootstrap_cluster path; leave room for every other step in it.
func TestAplPipelineBudgetsFitTheJob(t *testing.T) {
	const jobBudget = 70 * time.Minute
	var total time.Duration
	for _, st := range aplPipelineStages() {
		d, err := time.ParseDuration(st.condTimeout)
		if err != nil {
			t.Fatalf("stage %q: unparseable condTimeout %q: %v", st.desc, st.condTimeout, err)
		}
		total += st.existBudget + d
	}
	if total >= jobBudget {
		t.Errorf("stage budgets sum to %s, which meets or exceeds the job's %s timeout — a slow run would be axed by GitHub before any stage could report its own timeout", total, jobBudget)
	}
}

func TestWaitAplPipelineExistenceTimeoutFailsLoudWithDiagnostics(t *testing.T) {
	f := &fakeKubectl{responses: []kubectlRule{
		{match: "get crd/applications.argoproj.io", out: "", ok: false}, // never appears
	}}
	// step > budget so the deadline trips after the first poll.
	err := waitAplPipeline(aplPipelineStages(), aplTestDeps(f, 1000*time.Second))
	if err == nil || !strings.Contains(err.Error(), "applications.argoproj.io") {
		t.Fatalf("err = %v, want a hard failure naming the missing CRD", err)
	}
	if f.called("wait --for") {
		t.Error("must NOT issue a condition wait when the resource never appeared")
	}
	if !f.called("logs deploy/apl-operator") {
		t.Error("an existence timeout must dump apl-operator logs for diagnostics")
	}
}

func TestWaitAplPipelineConditionFailureFailsLoud(t *testing.T) {
	// CRDs/workloads exist, but the kyverno admission controller never goes
	// Available — the wait must surface as a hard error (no soft-fail).
	f := &fakeKubectl{responses: []kubectlRule{
		{match: "wait --for=condition=Available deployment/kyverno-admission-controller", out: "timed out waiting", ok: false},
	}}
	err := waitAplPipeline(aplPipelineStages(), aplTestDeps(f, time.Second))
	if err == nil || !strings.Contains(err.Error(), "kyverno-admission-controller") {
		t.Fatalf("err = %v, want a hard failure naming the kyverno deployment", err)
	}
	// cert-manager stages come AFTER kyverno, so they must not have run.
	if f.called("cert-manager") {
		t.Error("must abort at the failed stage, not continue to cert-manager")
	}
}

func TestWaitForAplResourceForClauseTranslation(t *testing.T) {
	// bare condition → --for=condition=<name>; clause with "=" passed verbatim.
	f := &fakeKubectl{}
	d := aplTestDeps(f, time.Second)
	if err := waitForAplResource(d, aplWaitStage{resource: "deployment/x", forClause: "Available", condTimeout: "5m"}); err != nil {
		t.Fatal(err)
	}
	if !f.called("wait --for=condition=Available deployment/x") {
		t.Errorf("bare condition not wrapped: %v", f.calls)
	}
	f2 := &fakeKubectl{}
	if err := waitForAplResource(aplTestDeps(f2, time.Second),
		aplWaitStage{namespace: "argocd", resource: "statefulset/y", forClause: "jsonpath={.status.readyReplicas}=1", condTimeout: "10m"}); err != nil {
		t.Fatal(err)
	}
	if !f2.called("-n argocd wait --for=jsonpath={.status.readyReplicas}=1 statefulset/y --timeout=10m") {
		t.Errorf("jsonpath clause not passed verbatim: %v", f2.calls)
	}
}

func TestRunCIWaitAplPipelineRequiresKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG_RAW", "")
	if err := runCIWaitAplPipeline(); err == nil || !strings.Contains(err.Error(), "KUBECONFIG_RAW") {
		t.Errorf("err = %v, want KUBECONFIG_RAW-required error", err)
	}
	if c := ciWaitAplPipelineCmd(); c.Use != "wait-apl-pipeline" {
		t.Errorf("Use = %q, want wait-apl-pipeline", c.Use)
	}
}
