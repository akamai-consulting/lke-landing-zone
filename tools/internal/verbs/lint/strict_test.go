package lint

// strict_test.go — `llz check <step> --strict` refuses a check that examined
// nothing.
//
// The default is a SKIP, and that default is correct for the pre-commit gate
// this package began as: an absent linter must never wedge a commit. In a
// delivered CI job it is the wrong answer and produces exactly the vacuous green
// the release-e2e PR-gate probe would then certify — `llz check tf-lint` on an
// instance whose TF_IMAGE lost tflint, or before `llz render` has laid the roots
// down, exits 0 having scanned nothing. Same command, two callers, opposite
// correct answers.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// runCheck drives `llz check <step>` in dir with the given flags.
func runCheck(t *testing.T, dir string, args ...string) error {
	t.Helper()
	t.Chdir(dir)
	c := CheckCmd()
	c.SetArgs(args)
	c.SetOut(os.Stderr)
	c.SilenceUsage, c.SilenceErrors = true, true
	return c.Execute()
}

// withNoTflint makes tflint unresolvable without touching PATH for anything else.
func withNoTflint(t *testing.T) {
	t.Helper()
	orig := kubectlprobe.LookPathFn
	t.Cleanup(func() { kubectlprobe.LookPathFn = orig })
	kubectlprobe.LookPathFn = func(bin string) (string, error) {
		if bin == "tflint" {
			return "", exec.ErrNotFound
		}
		return orig(bin)
	}
}

func TestStrictFailsWhenTheLinterIsMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "terraform-iac-bootstrap", "cluster"), 0o755); err != nil {
		t.Fatal(err)
	}
	withNoTflint(t)

	if err := runCheck(t, dir, "tf-lint"); err != nil {
		t.Fatalf("without --strict a missing tool must stay a skip — the pre-commit gate depends on it: %v", err)
	}
	err := runCheck(t, dir, "tf-lint", "--strict")
	if err == nil {
		t.Fatal("--strict passed with tflint absent: the gate reported success having scanned nothing, " +
			"and the e2e PR-gate probe would certify that success")
	}
	if !strings.Contains(err.Error(), "tflint") {
		t.Errorf("the error should name the missing tool, got: %v", err)
	}
}

// THE TREE THIS DRIVES IS THE ONE A REAL INSTANCE HAS, which is the whole point.
// An earlier version used a bare tempdir with no terraform-iac-bootstrap/ at all —
// a shape no checkout ever takes — and so could not fail for the case its own
// comment claimed: every instance HAS cluster/ and object-storage/, because each
// holds a tracked .terraform.lock.hcl, while carrying zero *.tf. A guard that
// counted directories saw "2 roots" and signed off on a tree with nothing in it
// to lint.
func TestStrictFailsWhenTheRootsHoldNoHCL(t *testing.T) {
	dir := t.TempDir()
	for _, root := range []string{"cluster", "object-storage"} {
		d := filepath.Join(dir, "terraform-iac-bootstrap", root)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		// The one tracked file an unrendered root really carries.
		if err := os.WriteFile(filepath.Join(d, ".terraform.lock.hcl"), []byte("# provider pins\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Stand tflint in as /usr/bin/true: this test is about the VACUITY GUARD, not
	// about tflint, and depending on a real linter being installed would make the
	// gate skip (haveTool -> false) in any environment without it — which is the
	// other half of what --strict exists to catch, and would quietly test the
	// wrong branch.
	t.Setenv("LLZ_TFLINT", "true")

	if err := runCheck(t, dir, "tf-lint"); err != nil {
		t.Fatalf("without --strict an unrendered tree must stay a pass: %v", err)
	}
	err := runCheck(t, dir, "tf-lint", "--strict")
	if err == nil {
		t.Fatal("--strict passed on an UNRENDERED instance tree — root directories present, zero *.tf. " +
			"That is exactly the shape the delivered lint jobs had before the render step was added, " +
			"and the vacuous green the e2e PR-gate probe would then certify")
	}
	if !strings.Contains(err.Error(), "llz render") {
		t.Errorf("the error should say how to get the roots, got: %v", err)
	}

	// And once the roots ARE rendered it must pass again, or the flag is just a
	// permanent failure wearing a gate's clothes.
	if err := os.WriteFile(filepath.Join(dir, "terraform-iac-bootstrap", "cluster", "main.tf"),
		[]byte("terraform {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runCheck(t, dir, "tf-lint", "--strict"); err != nil {
		t.Errorf("--strict still failed with rendered HCL present: %v", err)
	}
}

// The flag has to be reachable from the delivered call sites, which pass it after
// the step name.
func TestStrictIsAPersistentFlagOnCheck(t *testing.T) {
	if CheckCmd().PersistentFlags().Lookup("strict") == nil {
		t.Fatal("`llz check` lost --strict; the delivered tf-lint/checkov jobs pass it")
	}
	for _, step := range []string{"tf-lint", "checkov"} {
		var found bool
		for _, sub := range CheckCmd().Commands() {
			if sub.Use == step {
				found = true
			}
		}
		if !found {
			t.Errorf("`llz check %s` is gone — a delivered workflow calls it", step)
		}
	}
}

// The two empty trees need different words. "The root directories exist" is
// simply false for a checkout that has no terraform-iac-bootstrap/ at all, and
// sending someone to look for HCL inside directories they do not have wastes the
// first five minutes of the diagnosis.
func TestStrictDistinguishesAnUnrenderedTreeFromNoTreeAtAll(t *testing.T) {
	t.Setenv("LLZ_TFLINT", "true")

	noRoots := t.TempDir()
	err := runCheck(t, noRoots, "tf-lint", "--strict")
	if err == nil {
		t.Fatal("--strict must fail where there is nothing to scan")
	}
	if !strings.Contains(err.Error(), "no terraform-iac-bootstrap/ root here at all") {
		t.Errorf("a checkout with no roots should say so, got: %v", err)
	}
	if strings.Contains(err.Error(), "root directories exist") {
		t.Errorf("claimed the root directories exist where there are none: %v", err)
	}

	unrendered := t.TempDir()
	for _, root := range []string{"cluster", "object-storage"} {
		d := filepath.Join(unrendered, "terraform-iac-bootstrap", root)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, ".terraform.lock.hcl"), []byte("# pins\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	err = runCheck(t, unrendered, "tf-lint", "--strict")
	if err == nil {
		t.Fatal("--strict must fail on an unrendered instance tree")
	}
	if !strings.Contains(err.Error(), "root directories exist") {
		t.Errorf("an unrendered instance should be told the roots are there but empty, got: %v", err)
	}
}

// The delivered job renders with --if-spec, which NO-OPS without a spec — so on
// a spec-less instance with no committed Terraform, telling the operator to run
// that render sends them back to the command that just did nothing.
func TestStrictDoesNotSendASpecLessInstanceBackToTheRender(t *testing.T) {
	t.Setenv("LLZ_TFLINT", "true")
	dir := t.TempDir()
	d := filepath.Join(dir, "terraform-iac-bootstrap", "cluster")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, ".terraform.lock.hcl"), []byte("# pins\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runCheck(t, dir, "tf-lint", "--strict")
	if err == nil {
		t.Fatal("--strict must fail with nothing to scan")
	}
	if !strings.Contains(err.Error(), "NO LandingZone spec") {
		t.Errorf("a spec-less instance should be told the render cannot help it, got: %v", err)
	}
	if strings.Contains(err.Error(), "run it before this check") {
		t.Errorf("sent a spec-less instance back to a render that no-ops: %v", err)
	}
}

// --strict read as a property of `llz check` while protecting only tf-lint and
// checkov. stepGoFmt's `len(dirs) == 0 || !haveTool(...)` short-circuits, so a
// missing gofmt was never RECORDED and the vacuity census saw a clean pass; the
// same shape left actions-lint scanning no workflows and exiting 0.
func TestStrictAlsoCoversTheNonTerraformSteps(t *testing.T) {
	dir := t.TempDir() // no Go tree, no .github/workflows

	for _, step := range []string{"actions-lint"} {
		t.Setenv("LLZ_ACTIONLINT", "true")
		if err := runCheck(t, dir, step); err != nil {
			t.Fatalf("without --strict %s must stay a pass: %v", step, err)
		}
		if err := runCheck(t, dir, step, "--strict"); err == nil {
			t.Errorf("`llz check %s --strict` passed having examined nothing — the flag reads as a "+
				"property of `llz check`, so it has to hold for every step that scans a target set", step)
		}
	}
}
