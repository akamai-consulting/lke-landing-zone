package health

import "testing"

// A PDB whose pods are still ContainerCreating reports currentHealthy=0, which
// is exactly what a workload that will never come up reports. Under a budget
// that is a rollout, not a verdict.
//
// This is the shape that aborted a real e2e round on a cluster ~13 minutes old:
// `PDB monitoring/loki-ingester (currentHealthy=0 desiredHealthy=2
// disruptionsAllowed=0 expectedPods=3)` was the ONLY hard failure in the whole
// health tree, while the same poll reported all three loki-ingester pods as
// ContainerCreating and monitoring-loki as Progressing. It hard-failed because
// the branch was gated on phase1 ("OpenBao bootstrap pending"), which is one-way
// and had long since resolved — so nothing was left to absorb a later-wave
// chart's rollout.
func TestStillStartingPDBIsPendingUnderABudget(t *testing.T) {
	prev := Budgeted
	Budgeted = true
	defer func() { Budgeted = prev }()

	cat, msg := ClassifyPDB("monitoring/loki-ingester", 0, 2, 0, 3, false)
	if cat != CatPending {
		t.Fatalf("mid-rollout PDB under a budget = %v, want CatPending (msg: %s)", cat, msg)
	}
}

// The budget is what converts pending back into a verdict. Outside one — the
// one-shot `llz ci health` that scheduled cluster-health and the in-cluster
// reconciler run — the same observation must still fail, or LLZClusterNotConverged
// never fires. This is the regression budgeted.go exists to prevent.
func TestStillStartingPDBStillFailsOutsideABudget(t *testing.T) {
	prev := Budgeted
	Budgeted = false
	defer func() { Budgeted = prev }()

	cat, msg := ClassifyPDB("monitoring/loki-ingester", 0, 2, 0, 3, false)
	if cat != CatFail {
		t.Fatalf("unhealthy PDB outside a budget = %v, want CatFail", cat)
	}
	// desiredHealthy(2) < expectedPods(3): the budget permits drains as soon as
	// the workload heals, so sending the operator to minAvailable sends them to a
	// file with nothing wrong in it.
	if contains(msg, "check minAvailable vs replicas") {
		t.Errorf("a satisfiable budget must not blame minAvailable: %s", msg)
	}
	if !contains(msg, "BACKING WORKLOAD") {
		t.Errorf("message should name the workload as the verdict, got: %s", msg)
	}
}

// desiredHealthy >= expectedPods is the case the "check minAvailable vs replicas"
// remediation actually describes: no amount of healing raises disruptionsAllowed.
func TestUndrainableBudgetBlamesMinAvailable(t *testing.T) {
	prev := Budgeted
	Budgeted = false
	defer func() { Budgeted = prev }()

	cat, msg := ClassifyPDB("x/p", 2, 2, 0, 2, false)
	if cat != CatFail {
		t.Fatalf("minAvailable >= replicas = %v, want CatFail", cat)
	}
	if !contains(msg, "minAvailable ≥ replicas") {
		t.Errorf("message should blame minAvailable, got: %s", msg)
	}
}
