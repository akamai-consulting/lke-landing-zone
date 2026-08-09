package reconciler

import "testing"

// drivingenabled_test.go — the eight-way OR that decides whether this process
// needs to be the leader.
//
// It came from cmd/llz/uncovered_helpers_test.go, a file whose name told a reader
// nothing about what was in it — and what was in it was the single test that
// names every driving lane by hand. Filename-as-subject, tenth occurrence, and the
// costliest kind: this test's whole value is that it enumerates a set the code
// only ever ORs together, so it is exactly the test you would fail to notice had
// been left behind.

// drivingEnabled is an eight-way OR. Dropping any single disjunct silently
// disables that reconciler's ability to keep the loop driving, so each flag is
// asserted to be sufficient ON ITS OWN.
func TestDrivingEnabled(t *testing.T) {
	if (reconcileOpts{}).drivingEnabled() {
		t.Error("no flags set must not drive")
	}
	// reconcileOpenBao and reconcileTokens are deliberately NOT in the expression;
	// if that changes, the last subtest here should start failing. (The harbor lane
	// was dropped from drivingEnabled by #361 and reconcileVolTags added in its
	// place — this test named the exact disjunct set, so the rebase surfaced it.)
	for name, set := range map[string]func(*reconcileOpts){
		"argoNudge":  func(o *reconcileOpts) { o.reconcileArgoNudge = true },
		"cidrFW":     func(o *reconcileOpts) { o.reconcileCidrFW = true },
		"volLabels":  func(o *reconcileOpts) { o.reconcileVolLabels = true },
		"scDemote":   func(o *reconcileOpts) { o.reconcileSCDemote = true },
		"linodeCred": func(o *reconcileOpts) { o.reconcileLinodeCred = true },
		"volTags":    func(o *reconcileOpts) { o.reconcileVolTags = true },
		"esRecovery": func(o *reconcileOpts) { o.reconcileESRecovery = true },
		"aplOverlay": func(o *reconcileOpts) { o.reconcileAplOverlay = true },
	} {
		t.Run(name+" alone drives", func(t *testing.T) {
			var o reconcileOpts
			set(&o)
			if !o.drivingEnabled() {
				t.Errorf("%s alone must enable driving — it is a disjunct of drivingEnabled", name)
			}
		})
	}
	t.Run("openbao and tokens alone do not drive", func(t *testing.T) {
		o := reconcileOpts{reconcileOpenBao: true, reconcileTokens: true}
		if o.drivingEnabled() {
			t.Error("openbao/tokens are not driving reconcilers; if that changed, update drivingEnabled deliberately")
		}
	})
}
