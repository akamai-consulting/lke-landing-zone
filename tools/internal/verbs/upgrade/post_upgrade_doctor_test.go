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
	"os"
	"path/filepath"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/onboard"
)

// A SPLIT CONTRACT with doctor's own default: the upgrade's advisory and the
// `llz doctor` it tells the operator to re-run must ask about the SAME
// deployment. Both now resolve it through onboard.DefaultDoctorEnv, so the test
// is that neither has grown its own answer.
func TestPostUpgradeDoctorMatchesDoctorDefaults(t *testing.T) {
	if onboard.DoctorCmd().Flags().Lookup("env") == nil {
		t.Fatal("llz doctor has no --env flag — this gate would pass having compared nothing")
	}
	// THE DEPLOYMENT MUST COME FROM THE INSTANCE, not from a constant. `e2e` is the
	// template's own throwaway lane; a real adopter has prod / primary / staging.
	// Measured against a live instance mid-upgrade, the constant produced a
	// readiness report headed infra-e2e, a fix reading `llz tokens --env e2e --yes`
	// and a closing "run `llz env add e2e` first" — three wrong instructions
	// wrapped around one correct finding.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "terraform-iac-bootstrap", "cluster"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSpec(t, dir, "prod")
	t.Chdir(dir)

	if got := onboard.DefaultDoctorEnv(); got != "prod" {
		t.Errorf("in an instance whose only deployment is %q, doctor defaults to %q — "+
			"an adopter's readiness report would name a deployment they do not have", "prod", got)
	}
}

// With no spec there is nothing to resolve, and the old constant is still the
// right answer — a bare `llz doctor` outside an instance must not start failing.
func TestDoctorEnvFallsBackWithoutASpec(t *testing.T) {
	t.Chdir(t.TempDir())
	if got := onboard.DefaultDoctorEnv(); got != onboard.FallbackDoctorEnv {
		t.Errorf("outside an instance doctor resolved %q, want the %q fallback", got, onboard.FallbackDoctorEnv)
	}
}

// writeSpec lays down the minimum LandingZone spec that names one deployment.
// A deployment is authored as environments/<env>.yaml, never inline in
// landingzone.yaml — LoadSplit rejects the inline form outright, so a helper that
// wrote it there would resolve nothing and the assertion above would "pass" only
// because the fallback happened to be wrong in the same direction.
func writeSpec(t *testing.T, dir, env string) {
	t.Helper()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("landingzone.yaml", "apiVersion: llz.akamai-consulting.io/v1alpha1\n"+
		"kind: LandingZone\nmetadata:\n  name: test\nspec:\n  instance:\n    repo: o/r\n")
	write(filepath.Join("environments", env+".yaml"), "apiVersion: llz.akamai-consulting.io/v1alpha1\n"+
		"kind: ClusterDefinition\nmetadata:\n  name: "+env+"\nspec:\n  cluster:\n    region: us-ord\n")
}

// The advisory must stay advisory: a not-ready instance is not a failed upgrade,
// and returning the readiness error would fail a command whose work is already in
// the tree — breaking --commit after copier had landed its changes.
//
// THIS DRIVES THE REAL CODE PATH (finishUpgrade — the tail of Run), which is the
// only version of this test worth having. An earlier one substituted the seam and
// then called the SUBSTITUTE, so it asserted that a variable it had just assigned
// was assignable: it would have gone on passing had the call site started
// returning the readiness error, or stopped running --commit after it, which are
// the two ways this can actually break.
func TestPostUpgradeDoctorIsAdvisory(t *testing.T) {
	doctor, commits := stubUpgradeTail(t)

	err := finishUpgrade(false, true /*commit*/, false /*noDoctor*/, "v1", "v2", nil)

	if err != nil {
		t.Errorf("a readiness check that reported problems failed the upgrade (%v) — "+
			"the work is already in the operator's tree, so there is nothing for a non-zero exit to undo", err)
	}
	if *doctor != 1 {
		t.Errorf("the readiness check ran %d time(s), want 1", *doctor)
	}
	if *commits != 1 {
		t.Error("--commit did not commit after the readiness check reported problems — " +
			"the upgrade landed in the tree and was then left uncommitted, which is the regression " +
			"that made the check dangerous to add at all")
	}
}

// --no-doctor is the offline/no-gh-auth escape hatch, and --dry-run has rendered
// nothing to be ready for. Neither may reach the network.
func TestPostUpgradeDoctorIsSkippable(t *testing.T) {
	for _, tc := range []struct {
		name             string
		dryRun, noDoctor bool
	}{
		{"--no-doctor", false, true},
		{"--dry-run", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doctor, _ := stubUpgradeTail(t)
			if err := finishUpgrade(tc.dryRun, false, tc.noDoctor, "v1", "v2", nil); err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if *doctor != 0 {
				t.Errorf("%s still ran the readiness check — it reaches gh and the Linode API", tc.name)
			}
		})
	}
}

// And without --commit nothing is committed: `llz upgrade` must never quietly
// commit someone's working tree.
func TestUpgradeDoesNotCommitWithoutTheFlag(t *testing.T) {
	_, commits := stubUpgradeTail(t)
	if err := finishUpgrade(false, false /*commit*/, true, "v1", "v2", nil); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if *commits != 0 {
		t.Error("the upgrade committed without --commit")
	}
}

// stubUpgradeTail swaps the two seams finishUpgrade drives — the readiness check
// (gh + the Linode API) and the commit (`git add -A` in the caller's tree) — and
// returns their call counters. The doctor stub stands in for a check that found
// problems, since that is the only interesting case: a clean one proves nothing
// about whether the error is swallowed.
func stubUpgradeTail(t *testing.T) (doctor, commits *int) {
	t.Helper()
	origDoctor, origCommit := runPostUpgradeDoctor, commitUpgrade
	t.Cleanup(func() { runPostUpgradeDoctor, commitUpgrade = origDoctor, origCommit })

	doctor, commits = new(int), new(int)
	runPostUpgradeDoctor = func() { *doctor++ }
	commitUpgrade = func(bool, string, string) error { *commits++; return nil }
	return doctor, commits
}

// --no-doctor has to remain reachable: it is the escape hatch for upgrading
// offline or without gh auth, which is the whole reason the check is optional.
func TestUpgradeExposesNoDoctorFlag(t *testing.T) {
	if UpgradeCmd().Flags().Lookup("no-doctor") == nil {
		t.Error("`llz upgrade` lost --no-doctor — an offline upgrade now has no way to skip a check that needs the network")
	}
}
