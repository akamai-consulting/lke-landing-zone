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
	// All six stages: CRD Established → workload readiness, in order.
	for _, want := range []string{
		"get crd/applications.argoproj.io",
		// StatefulSet readiness is a jsonpath --for clause passed verbatim (not condition=).
		"wait --for=jsonpath={.status.readyReplicas}=1 statefulset/argocd-application-controller --timeout=10m",
		"wait --for=condition=Available deployment/kyverno-admission-controller --timeout=5m",
		"-n cert-manager wait --for=condition=Available deployment/cert-manager-webhook --timeout=5m",
		"wait --for=condition=Established crd/certificates.cert-manager.io --timeout=5m",
	} {
		if !f.called(want) {
			t.Errorf("expected kubectl call containing %q\ncalls: %v", want, f.calls)
		}
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
