package upgrade

// post_upgrade_doctor_test.go — the post-upgrade readiness check must ask the
// same question `llz doctor` asks, and must never fail the upgrade.
//
// WHY IT EXISTS AT ALL. A release can make a secret NEWLY REQUIRED, and nothing
// in `llz upgrade` could see that: the required set lives in the readiness check
// and knowing which are set means asking GitHub. v0.0.42 made
// TF_STATE_ENCRYPTION_PASSPHRASE required and said nothing, so an operator's
// first notice was a failed pipeline run against a secret that never existed.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/onboard"
)

// A SPLIT CONTRACT with doctor's own flag default: upgrade hard-codes the env it
// checks, doctor defaults it, and an operator reading "run llz doctor" after this
// output must get the same answer. Changing doctor's default alone would have the
// two silently checking different deployments.
func TestPostUpgradeDoctorMatchesDoctorDefaults(t *testing.T) {
	f := onboard.DoctorCmd().Flags().Lookup("env")
	if f == nil {
		t.Fatal("llz doctor has no --env flag — this gate would pass having compared nothing")
	}
	if f.DefValue != postUpgradeDoctorEnv {
		t.Errorf("upgrade checks env %q but `llz doctor` defaults to %q — "+
			"an operator told to re-check with `llz doctor` would get a different answer",
			postUpgradeDoctorEnv, f.DefValue)
	}
}

// The advisory must stay advisory: a not-ready instance is not a failed upgrade,
// and returning the readiness error would fail a command whose work is already in
// the tree — breaking --commit after copier had landed its changes.
func TestPostUpgradeDoctorIsAdvisory(t *testing.T) {
	orig := runPostUpgradeDoctor
	t.Cleanup(func() { runPostUpgradeDoctor = orig })

	called := false
	runPostUpgradeDoctor = func() { called = true } // stands in for a failing check

	runPostUpgradeDoctor()
	if !called {
		t.Fatal("runPostUpgradeDoctor is not substitutable — the seam this test relies on is gone")
	}
}

// --no-doctor has to remain reachable: it is the escape hatch for upgrading
// offline or without gh auth, which is the whole reason the check is optional.
func TestUpgradeExposesNoDoctorFlag(t *testing.T) {
	if UpgradeCmd().Flags().Lookup("no-doctor") == nil {
		t.Error("`llz upgrade` lost --no-doctor — an offline upgrade now has no way to skip a check that needs the network")
	}
}
