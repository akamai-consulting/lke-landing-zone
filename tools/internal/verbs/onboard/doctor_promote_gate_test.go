package onboard

// doctor_promote_gate_test.go — the gate for the promote.yml arm of
// checkSpecPreflights.
//
// IT ASSERTS THE FINDING, NOT THE WIRING. A test that only checked "doctor calls
// PlanWorkflow" would pass against a doctor that swallowed the verdict, and
// swallowing it is exactly the defect: the check already EXISTED as a CI job and
// nothing local ever asked it. So these build a tree that is unrunnable in the one
// way that matters and require doctor's exit status to carry it.
//
// The instance fixture is default_doctor_env_test.go's writeInstance — the same
// split spec every other test in this package reasons about, rather than a second
// hand-rolled tree that could drift into agreeing with itself.

import (
	"path/filepath"
	"strings"
	"testing"
)

// staleExamplePromote is the three-stage pipeline the template shipped as a
// runnable example before v0.0.45, cut down to the lines the check reads. `dev`
// and `staging` are declared nowhere; dispatching this dies three jobs deep on
// `llz: env "dev" not in spec`.
const staleExamplePromote = `name: Promote
on: {workflow_dispatch: {}}
jobs:
  dev:
    uses: ./.github/workflows/llz-terraform.yml
    with:
      instance_repo: acme/inst
      action: apply
      region: dev
  staging:
    needs: dev
    uses: ./.github/workflows/llz-terraform.yml
    with:
      instance_repo: acme/inst
      action: apply
      region: staging
  prod:
    needs: staging
    uses: ./.github/workflows/llz-terraform.yml
    with:
      instance_repo: acme/inst
      action: apply
      region: prod
`

// singleStagePromote names only `prod`, which the fixture declares.
const singleStagePromote = `name: Promote
on: {workflow_dispatch: {}}
jobs:
  prod:
    uses: ./.github/workflows/llz-terraform.yml
    with:
      instance_repo: acme/inst
      action: apply
      region: prod
`

// promoteFixture is a one-deployment instance — the gsap-apl shape — with the
// given promote.yml on disk ("" writes none).
func promoteFixture(t *testing.T, promoteYAML string) {
	t.Helper()
	dir := chdirTempDir(t)
	writeInstance(t, dir, "prod")
	if promoteYAML != "" {
		mustWrite(t, filepath.Join(dir, ".github", "workflows", "promote.yml"), promoteYAML)
	}
}

// TestDoctorFailsOnUndeclaredPromotionStage is the regression this arm exists for.
// An adopter instance carried the shipped three-stage example over a spec
// declaring only `prod` from the day it was scaffolded. `llz doctor` — and
// `llz upgrade`, which runs doctor as its post-upgrade readiness report — stayed
// green for months, until an unrelated upgrade PR met the CI job. Doctor has to
// reach CI's verdict here, because the two are reading the same two files off the
// same disk.
func TestDoctorFailsOnUndeclaredPromotionStage(t *testing.T) {
	promoteFixture(t, staleExamplePromote)

	var errs []error
	out := captureStdout(t, func() { errs = checkSpecPreflights("") })
	if len(errs) == 0 {
		t.Fatalf("doctor must FAIL on a promote.yml naming deployments the spec does not declare; it printed:\n%s", out)
	}
	got := errs[0].Error()
	// The undeclared stages BY NAME — an operator cannot act on a count.
	for _, want := range []string{`"dev"`, `"staging"`} {
		if !strings.Contains(got, want) {
			t.Errorf("finding must name the undeclared stage %s, got:\n%s", want, got)
		}
	}
	// The deployments that DO exist are the part not recoverable from the error
	// alone, and they are what decides whether the fix is `llz env add` or
	// `llz env pipeline`.
	if !strings.Contains(got, "declared deployments: prod") {
		t.Errorf("finding must list the declared deployments, got:\n%s", got)
	}
	// The remediation travels with the refusal — this repo's standard.
	if !strings.Contains(got, "llz env pipeline") {
		t.Errorf("finding must carry the remedy, got:\n%s", got)
	}
	// And the operator has to SEE it. The error lands in the exit status; the
	// section is what is in front of them.
	if !strings.Contains(out, "promotion pipeline") {
		t.Errorf("doctor must print the promotion-pipeline line, got:\n%s", out)
	}
}

// TestDoctorPassesOnDeclaredPromotionStages is the other half of the gate, and it
// is not decoration: an arm that failed unconditionally satisfies the test above
// while making `llz doctor` unusable, and a gate that only ever asserts the red
// verdict cannot tell the two apart.
func TestDoctorPassesOnDeclaredPromotionStages(t *testing.T) {
	// One applying stage, so the ordering check has nothing to compare — it must
	// not invent a failure out of that.
	promoteFixture(t, singleStagePromote)

	var errs []error
	out := captureStdout(t, func() { errs = checkSpecPreflights("") })
	if len(errs) != 0 {
		t.Fatalf("a promote.yml naming only declared deployments must pass, got %v\n%s", errs, out)
	}
}

// TestDoctorQuietWithoutPromoteWorkflow pins the abstention that IS correct. No
// promote.yml is a real state — nothing has generated one yet — and a file that
// does not exist makes no claim to falsify. Failing here would fail every fresh
// instance with no reachable remedy, which is this bug's mirror image.
func TestDoctorQuietWithoutPromoteWorkflow(t *testing.T) {
	promoteFixture(t, "")

	var errs []error
	captureStdout(t, func() { errs = checkSpecPreflights("") })
	if len(errs) != 0 {
		t.Fatalf("an instance with no promote.yml must not fail doctor, got %v", errs)
	}
}
