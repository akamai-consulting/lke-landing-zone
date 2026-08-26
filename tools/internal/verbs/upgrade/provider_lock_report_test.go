package upgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/providerlock"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfroots"
)

// The provider-lock REPORT test, the sibling of image_skew_report_test.go and
// for the same reason: what `llz upgrade` PRINTS is the entire deliverable of
// this half. It cannot regenerate the lock (the file is `owned`, and which
// providers an instance pins is the instance's decision), so the warning is all
// the operator gets before a red check on the pull request tells them 20 minutes
// later.
//
// THE SKEW IS BUILT FROM THE REAL EMBEDDED CONSTRAINTS, unlike providerlock's own
// tests, which pin a fixture so a dependabot bump cannot move them. The property
// here is different: this asserts the reporter fires on the tree an operator
// really has, and a fixture constraint would prove only that the reporter can
// print — including on a release where the wiring to the real roots had been cut.
func TestReportProviderLockSkew(t *testing.T) {
	// stalePins writes a lock that violates whatever the shipped roots constrain
	// today. 0.0.1 is below every constraint the roots have ever carried, so this
	// needs no table of provider history.
	stalePins := func(t *testing.T, dir, root string) {
		t.Helper()
		constraints := providerlock.ParseConstraints(tfroots.RootVersions()[root], root)
		if len(constraints) == 0 {
			t.Fatalf("the shipped %s root constrains no provider, so this warning can never fire — "+
				"that is a finding about the roots, not a reason to skip", root)
		}
		var b strings.Builder
		for _, c := range constraints {
			b.WriteString("provider \"registry.opentofu.org/" + c.Provider + "\" {\n" +
				"  version     = \"0.0.1\"\n  constraints = \"~> 0.0\"\n}\n\n")
		}
		d := filepath.Join(dir, providerlock.InstanceLockDir, root)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, providerlock.LockFile), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("names the root, the pin, and the command that fixes it", func(t *testing.T) {
		dir := t.TempDir()
		chdir(t, dir)
		stalePins(t, dir, "cluster")

		_, errOut := captureStdoutStderr(t, func() { reportProviderLockSkew() })

		for _, want := range []string{
			"cluster",                  // which root
			"0.0.1",                    // what they have
			providerlock.RegenerateCmd, // the command, from the guard's own constant
			"terraform-iac-bootstrap/cluster",
			"provider-lock-guard", // names the check that will otherwise say it in CI
		} {
			if !strings.Contains(errOut, want) {
				t.Errorf("warning is missing %q:\n%s", want, errOut)
			}
		}
	})

	// Silence is load-bearing: this runs on EVERY upgrade, and most upgrades move
	// no provider constraint at all. A warning that fires when nothing is wrong is
	// one operators learn to scroll past — which would cost the release where it
	// was right.
	t.Run("prints nothing when the committed pins already satisfy the constraints", func(t *testing.T) {
		dir := t.TempDir()
		chdir(t, dir)
		// The lock tofu itself would write: exactly what the root constrains, in the
		// form the guard reads.
		constraints := providerlock.ParseConstraints(tfroots.RootVersions()["cluster"], "cluster")
		var b strings.Builder
		for _, c := range constraints {
			floor := strings.TrimPrefix(strings.TrimSpace(c.Spec), "~>")
			b.WriteString("provider \"registry.opentofu.org/" + c.Provider + "\" {\n" +
				"  version     = \"" + strings.TrimSpace(floor) + ".0\"\n" +
				"  constraints = \"" + c.Spec + "\"\n}\n\n")
		}
		d := filepath.Join(dir, providerlock.InstanceLockDir, "cluster")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, providerlock.LockFile), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}

		out, errOut := captureStdoutStderr(t, func() { reportProviderLockSkew() })
		if out != "" || errOut != "" {
			t.Errorf("warned about pins that satisfy the shipped constraints:\nstdout=%q\nstderr=%q", out, errOut)
		}
	})

	// `llz upgrade` runs in trees that are not instances (and in instances that
	// commit no lock at all, which is a supported choice). Neither has anything to
	// say, and neither may produce a warning about pins nobody could examine.
	t.Run("prints nothing outside an instance Terraform tree", func(t *testing.T) {
		chdir(t, t.TempDir())
		out, errOut := captureStdoutStderr(t, func() { reportProviderLockSkew() })
		if out != "" || errOut != "" {
			t.Errorf("warned outside an instance:\nstdout=%q\nstderr=%q", out, errOut)
		}
	})
}

// TestEveryUpgradeReporterIsReached pins the WIRING, which nothing did.
//
// Each of these reporters had a test proving it prints the right thing, and none
// proved it RUNS. Deleting a call from Run left the whole suite green — an
// operator would simply stop being told, which is indistinguishable from having
// nothing to be told. That is the same failure shape the reporters exist to
// catch, so it is worth a gate of its own.
//
// The set is asserted, not just the count: a reporter added later has to be wired in
// here to pass, and a call quietly dropped names itself in the failure.
func TestEveryUpgradeReporterIsReached(t *testing.T) {
	reached := map[string]bool{}
	restore := func(image func(string) string, lock, prefix, promo, deploys func() string) {
		reportCIImageSkew, reportProviderLockSkew = image, lock
		reportUnpinnedObjLabelPrefix, reportUnrunnablePromotionPipeline = prefix, promo
		reportDeploymentsToApply = deploys
	}
	defer restore(reportCIImageSkew, reportProviderLockSkew, reportUnpinnedObjLabelPrefix,
		reportUnrunnablePromotionPipeline, reportDeploymentsToApply)

	var gotRef string
	reportCIImageSkew = func(ref string) string { reached["ci-image-skew"] = true; gotRef = ref; return "step: image" }
	reportProviderLockSkew = func() string { reached["provider-lock-skew"] = true; return "step: lock" }
	reportUnpinnedObjLabelPrefix = func() string { reached["obj-label-prefix"] = true; return "" }
	reportUnrunnablePromotionPipeline = func() string { reached["promotion-pipeline"] = true; return "step: pipeline" }
	reportDeploymentsToApply = func() string { reached["deployments-to-apply"] = true; return "step: apply" }

	steps := reportWhatTheUpgradeCouldNotDo("v9.9.9")
	// THE CHECKLIST IS BUILT FROM WHAT EACH REPORTER RETURNED, and a reporter with
	// nothing to say contributes nothing — otherwise the last screen of an upgrade
	// lists steps that do not apply, which is how a checklist stops being read.
	want := []string{"step: image", "step: lock", "step: pipeline", "step: apply"}
	if strings.Join(steps, "|") != strings.Join(want, "|") {
		t.Errorf("next steps = %v, want %v — the reporter that returned \"\" must contribute "+
			"nothing, and the order must be the order the operator has to act in", steps, want)
	}

	for _, want := range []string{"ci-image-skew", "provider-lock-skew", "obj-label-prefix",
		"promotion-pipeline", "deployments-to-apply"} {
		if !reached[want] {
			t.Errorf("%s was never reached — an operator would simply stop being told", want)
		}
	}
	// The ref is the one thing threaded through, and passing the wrong one would
	// have it report skew against a version nobody is moving to.
	if gotRef != "v9.9.9" {
		t.Errorf("the new pin reached the image-skew reporter as %q, want v9.9.9", gotRef)
	}
}

// TestAnUnreadableLockIsReportedNotSwallowed.
//
// ScanInstance hard-errors on a lock it cannot parse and returns NO partial
// results, so one bad file hides every other root's verdict. Swallowing that made
// `llz upgrade` print nothing — which an operator reads as "my pins are fine" —
// and it did so NON-DETERMINISTICALLY, because the scan walks a map: the same tree
// could warn or stay silent between runs. A warning that is missing at random is
// worse than one that is missing always, because nobody can reproduce it.
func TestAnUnreadableLockIsReportedNotSwallowed(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	d := filepath.Join(dir, providerlock.InstanceLockDir, "cluster")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, providerlock.LockFile),
		[]byte("# a lock this gate cannot read\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, errOut := captureStdoutStderr(t, func() { reportProviderLockSkew() })
	if errOut == "" {
		t.Fatal("a lock that could not be read produced no output at all — the operator reads " +
			"that as a clean bill of health for pins nothing examined")
	}
	if !strings.Contains(errOut, "provider-lock-guard") {
		t.Errorf("the note must name the check that will say the same thing in CI; got:\n%s", errOut)
	}
}

// TestReportUnpinnedObjLabelPrefix.
//
// The warning exists because of WHEN the alternative arrives: nothing else
// notices an about-to-happen bucket rename until an apply plans one, which is
// after the upgrade PR has merged and twenty minutes into a run that is then
// refused. The silence arm is the load-bearing one — this fires on every upgrade
// of an instance that has not pinned the field, so it must be silent for everyone
// who has.
func TestReportUnpinnedObjLabelPrefix(t *testing.T) {
	spec := func(t *testing.T, body string) {
		t.Helper()
		dir := t.TempDir()
		chdir(t, dir)
		writeFile(t, filepath.Join(dir, "landingzone.yaml"), body)
	}

	t.Run("warns, names the prefix it would use, and points at the real check", func(t *testing.T) {
		spec(t, "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata:\n  name: gsap-apl\nspec:\n  instance:\n    repo: acme/gsap-apl\n")
		_, errOut := captureStdoutStderr(t, func() { reportUnpinnedObjLabelPrefix() })
		for _, want := range []string{"objLabelPrefix", "gsap-apl-*", "platform", "create-time only",
			"llz ci assert-upgrade-plan", "bucket-prefix-rename.md"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("warning is missing %q:\n%s", want, errOut)
			}
		}
		// IT MUST NOT RECOMMEND A VALUE. Deciding which of the account's buckets are
		// this instance's from their labels alone is the guess that, made wrong,
		// renames the buckets holding the data. assert-upgrade-plan answers it from a
		// real plan plus live object counts; this only says to go and look.
		if strings.Contains(errOut, "objLabelPrefix: platform") {
			t.Errorf("this warning has no evidence to recommend a prefix and must not print one:\n%s", errOut)
		}
	})

	t.Run("silent when the instance pinned the prefix deliberately", func(t *testing.T) {
		spec(t, "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata:\n  name: gsap-apl\nspec:\n  instance:\n    repo: acme/gsap-apl\n    objLabelPrefix: platform\n")
		out, errOut := captureStdoutStderr(t, func() { reportUnpinnedObjLabelPrefix() })
		if out != "" || errOut != "" {
			t.Errorf("an instance that pinned the field has nothing to be told:\nstdout=%q\nstderr=%q", out, errOut)
		}
	})

	t.Run("silent with no spec at all", func(t *testing.T) {
		chdir(t, t.TempDir())
		out, errOut := captureStdoutStderr(t, func() { reportUnpinnedObjLabelPrefix() })
		if out != "" || errOut != "" {
			t.Errorf("a checkout with no spec renders no tfvars from one either:\nstdout=%q\nstderr=%q", out, errOut)
		}
	})
}

// TestTheChecklistNamesTheDeploymentRatherThanAPlaceholder.
//
// `llz tokens --env <env> --yes` was the FIRST line of the checklist an operator
// is meant to work through, with a hole in it. The provider-lock remedy carried
// `<root>` in the same way and for the same reason — nobody read it as an
// instruction until they had to run it. The deployment is on disk; a command
// printed as a next step should be one you can paste.
func TestTheChecklistNamesTheDeploymentRatherThanAPlaceholder(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	writeFile(t, filepath.Join(dir, "landingzone.yaml"),
		"apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata:\n  name: acme\n")
	writeFile(t, filepath.Join(dir, "environments", "prod.yaml"), "cluster: {}\n")
	writeFile(t, filepath.Join(dir, "terraform-iac-bootstrap", "cluster", "prod.tfvars"), "region = \"x\"\n")

	envs := instanceDeployments()
	if len(envs) != 1 || envs[0] != "prod" {
		t.Skipf("this fixture did not produce one deployment (%v); the assertion below needs exactly one", envs)
	}
	if got := oneOrPlaceholder(envs); got != "prod" {
		t.Errorf("a single deployment must be named, got %q", got)
	}
	// Two deployments IS a guess, and naming one would be wrong half the time.
	if got := oneOrPlaceholder([]string{"prod", "staging"}); got != "<env>" {
		t.Errorf("with several deployments the command must not pick one, got %q", got)
	}
	if got := oneOrPlaceholder(nil); got != "<env>" {
		t.Errorf("with no deployment there is nothing to name, got %q", got)
	}
}
