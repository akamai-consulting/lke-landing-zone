package health

import (
	"testing"
	"time"
)

func TestSupersededFailedJobs(t *testing.T) {
	base := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }

	jobs := []JobRun{
		// harbor-robot-provisioner: two early failures, then a later success →
		// the early failures are superseded (the recurring false-red pattern).
		{Key: "harbor/hrp-1", CronOwner: "hrp", Created: at(0), Failed: true},
		{Key: "harbor/hrp-2", CronOwner: "hrp", Created: at(5), Failed: true},
		{Key: "harbor/hrp-3", CronOwner: "hrp", Created: at(10), Complete: true},
		// nudger: earlier SUCCESS then a LATER failure → a current regression,
		// must NOT be masked.
		{Key: "argocd/nudge-1", CronOwner: "nudge", Created: at(0), Complete: true},
		{Key: "argocd/nudge-2", CronOwner: "nudge", Created: at(5), Failed: true},
		// a non-CronJob Job that failed → never masked (no owner).
		{Key: "kube-system/oneoff", CronOwner: "", Created: at(0), Failed: true},
	}
	got := SupersededFailedJobs(jobs)
	for _, want := range []string{"harbor/hrp-1", "harbor/hrp-2"} {
		if !got[want] {
			t.Errorf("%s should be superseded by the later successful sibling", want)
		}
	}
	for _, notWant := range []string{"argocd/nudge-2", "kube-system/oneoff"} {
		if got[notWant] {
			t.Errorf("%s must NOT be masked (current regression / no CronJob owner)", notWant)
		}
	}
	if len(got) != 2 {
		t.Errorf("expected exactly 2 superseded jobs, got %v", got)
	}
}

func TestClassifyPVPhase(t *testing.T) {
	for _, p := range []string{"Failed", "Pending"} {
		if ClassifyPVPhase(p) != CatFail {
			t.Errorf("%s PV should fail", p)
		}
	}
	for _, p := range []string{"Bound", "Available", "Released"} {
		if ClassifyPVPhase(p) != CatOK {
			t.Errorf("%s PV should be OK", p)
		}
	}
	if ClassifyPVPhase("Weird") != CatWarn {
		t.Error("unrecognized phase should warn")
	}
}

func TestClassifyNamespaceNetpol(t *testing.T) {
	if cat, _ := ClassifyNamespaceNetpol("openbao", 2); cat != CatOK {
		t.Error(">=1 NP should pass")
	}
	if cat, _ := ClassifyNamespaceNetpol("observability", 0); cat != CatFail {
		t.Error("0 NPs in a non-deferred namespace should fail")
	}
	if !NetpolExemptNamespace("argocd") || !NetpolExemptNamespace("kube-system") || NetpolExemptNamespace("openbao") {
		t.Error("netpol exemption set wrong")
	}
}

func TestClassifyJob(t *testing.T) {
	if cat, _ := ClassifyJob("x/j", true, false, 0, 1, 0, false); cat != CatOK {
		t.Error("complete job ok")
	}
	if cat, _ := ClassifyJob("x/j", false, true, 0, 0, 1, false); cat != CatFail {
		t.Error("failed job fails")
	}
	if cat, _ := ClassifyJob("openbao/j", false, true, 0, 0, 1, true); cat != CatPending {
		t.Error("failed job under phase-1 cascade pends")
	}
	if cat, _ := ClassifyJob("x/j", false, false, 0, 0, 0, false); cat != CatWarn {
		t.Error("never-started job warns")
	}
	if cat, _ := ClassifyJob("x/j", false, false, 1, 0, 0, false); cat != CatOK {
		t.Error("active job is in-progress (ok)")
	}
}

func TestClassifyCronWorkflow(t *testing.T) {
	if cat, _ := ClassifyCronWorkflow("o/cw", "parse error", false, 1, 30); cat != CatFail {
		t.Error("submission error fails")
	}
	if cat, _ := ClassifyCronWorkflow("o/cw", "", true, 1, 30); cat != CatWarn {
		t.Error("suspended warns")
	}
	if cat, _ := ClassifyCronWorkflow("o/cw", "", false, -1, 30); cat != CatOK {
		t.Error("never-scheduled is ok/info")
	}
	if cat, _ := ClassifyCronWorkflow("o/cw", "", false, 45, 30); cat != CatFail {
		t.Error("stale schedule fails")
	}
	if cat, _ := ClassifyCronWorkflow("o/cw", "", false, 5, 30); cat != CatOK {
		t.Error("recent schedule ok")
	}
}

func TestClassifyServiceEndpoints(t *testing.T) {
	if cat, _ := ClassifyServiceEndpoints("x/s", 2, false); cat != CatOK {
		t.Error("ready endpoints ok")
	}
	if cat, _ := ClassifyServiceEndpoints("openbao/s", 0, true); cat != CatPending {
		t.Error("0 endpoints under phase-1 pends")
	}
	if cat, _ := ClassifyServiceEndpoints("external-dns/external-dns", 0, false); cat != CatDeferred {
		t.Error("0 endpoints on a deferred workload defers")
	}
	if cat, _ := ClassifyServiceEndpoints("x/s", 0, false); cat != CatFail {
		t.Error("0 endpoints otherwise fails")
	}
}

func TestClassifyPDB(t *testing.T) {
	cases := []struct {
		name                 string
		cur, des, allow, exp int
		phase1               bool
		want                 Category
	}{
		{"orphan", 0, 0, 0, 0, false, CatOK},
		{"healthy with disruptions", 3, 2, 1, 3, false, CatOK},
		{"single replica", 1, 1, 0, 1, false, CatOK},
		{"over-provisioned", 2, 1, 0, 3, false, CatOK},
		{"phase1 settling", 0, 1, 0, 2, true, CatPending},
		{"misconfigured", 1, 2, 0, 2, false, CatFail},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := ClassifyPDB("ns/p", c.cur, c.des, c.allow, c.exp, c.phase1)
			if got != c.want {
				t.Errorf("ClassifyPDB = %v, want %v", got, c.want)
			}
		})
	}
	// Operator-deferred workload PDB stuck at currentHealthy=0 defers.
	if cat, _ := ClassifyPDB("external-dns/external-dns", 0, 1, 0, 1, false); cat != CatDeferred {
		t.Error("deferred workload PDB should defer")
	}
}

func TestClassifyIngress(t *testing.T) {
	if cat, _ := ClassifyIngress("x/i", 1, false); cat != CatOK {
		t.Error("programmed ingress ok")
	}
	if cat, _ := ClassifyIngress("x/i", 0, true); cat != CatPending {
		t.Error("no address under phase-1 pends")
	}
	if cat, _ := ClassifyIngress("x/i", 0, false); cat != CatFail {
		t.Error("no address otherwise fails")
	}
}

func TestClassifyWorkflowPhase(t *testing.T) {
	if cat, _ := ClassifyWorkflowPhase("x/w", "Failed", false); cat != CatFail {
		t.Error("failed workflow fails")
	}
	if cat, _ := ClassifyWorkflowPhase("x/w", "Error", true); cat != CatPending {
		t.Error("errored workflow under phase-1 pends")
	}
	if cat, _ := ClassifyWorkflowPhase("x/w", "Succeeded", false); cat != CatOK {
		t.Error("succeeded ok")
	}
	if cat, _ := ClassifyWorkflowPhase("x/w", "Running", false); cat != CatOK {
		t.Error("running is in-flight ok")
	}
	// Ephemeral e2e probe: ignored regardless of phase (a prior run's dead probe
	// on a reused cluster must not gate converge).
	if cat, _ := ClassifyWorkflowPhase("llz-argo-workflows/e2e-assert-health-vrvbr", "Failed", false); cat != CatOK {
		t.Error("a FAILED e2e-assert-health probe must be CatOK (ignored), not CatFail")
	}
	if cat, _ := ClassifyWorkflowPhase("llz-argo-workflows/e2e-assert-health-xyz", "Error", false); cat != CatOK {
		t.Error("an ERRORED e2e probe must be ignored")
	}
}

func TestIsEphemeralE2EProbe(t *testing.T) {
	for _, y := range []string{"e2e-assert-health-vrvbr", "e2e-assert-health-", "e2e-assert-health-abc123"} {
		if !IsEphemeralE2EProbe(y) {
			t.Errorf("%q should be an ephemeral e2e probe", y)
		}
	}
	for _, n := range []string{"llz-cluster-health", "e2e-assert", "cert-automation-xyz", ""} {
		if IsEphemeralE2EProbe(n) {
			t.Errorf("%q should NOT be an ephemeral e2e probe", n)
		}
	}
}

func TestStuckFinalizer(t *testing.T) {
	if !StuckFinalizer(true, 1, 600) {
		t.Error("deletion + finalizer + >300s should be stuck")
	}
	if StuckFinalizer(true, 1, 120) {
		t.Error("<300s is not yet stuck")
	}
	if StuckFinalizer(false, 1, 600) {
		t.Error("no deletionTimestamp is not stuck")
	}
	if StuckFinalizer(true, 0, 600) {
		t.Error("no finalizers is not stuck")
	}
	if len(StuckResourceKinds()) == 0 {
		t.Error("stuck-resource kind set should be non-empty")
	}
}
