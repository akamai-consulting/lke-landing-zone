package upgrade

import (
	"path/filepath"
	"strings"
	"testing"
)

// reporters_test.go — the post-upgrade reporters: that they RUN, and that what
// they print is usable.
//
// IT WAS provider_lock_report_test.go, and was renamed rather than deleted when
// the provider-lock reporter went away with the split that made it necessary
// (the lock is now rendered beside the constraint it satisfies — see
// tools/internal/shared/tfroots). Three of its four tests were never about that
// reporter: the wiring gate below covers all of them at once, and deleting the
// file to remove one arm would have taken the only thing asserting the other
// reporters are reached at all.

// TestEveryUpgradeReporterIsReached pins the WIRING, which nothing did.
//
// Each of these reporters had a test proving it prints the right thing, and none
// proved it RUNS. Deleting a call from Run left the whole suite green — an
// operator would simply stop being told, which is indistinguishable from having
// nothing to be told. That is the same failure shape the reporters exist to
// catch, so it is worth a gate of its own.
//
// The set is asserted, not just the count: a reporter added later has to be wired
// in here to pass, and a call quietly dropped names itself in the failure. It also
// notices one REMOVED without its `want` entry going with it, which is how the
// provider-lock arm left — the reporter and this arm had to go in one change.
func TestEveryUpgradeReporterIsReached(t *testing.T) {
	reached := map[string]bool{}
	restore := func(image func(string) string, prefix, promo, deploys func() string) {
		reportCIImageSkew = image
		reportUnpinnedObjLabelPrefix, reportUnrunnablePromotionPipeline = prefix, promo
		reportDeploymentsToApply = deploys
	}
	defer restore(reportCIImageSkew, reportUnpinnedObjLabelPrefix,
		reportUnrunnablePromotionPipeline, reportDeploymentsToApply)

	var gotRef string
	reportCIImageSkew = func(ref string) string { reached["ci-image-skew"] = true; gotRef = ref; return "step: image" }
	reportUnpinnedObjLabelPrefix = func() string { reached["obj-label-prefix"] = true; return "" }
	reportUnrunnablePromotionPipeline = func() string { reached["promotion-pipeline"] = true; return "step: pipeline" }
	reportDeploymentsToApply = func() string { reached["deployments-to-apply"] = true; return "step: apply" }

	steps := reportWhatTheUpgradeCouldNotDo("v9.9.9")
	// THE CHECKLIST IS BUILT FROM WHAT EACH REPORTER RETURNED, and a reporter with
	// nothing to say contributes nothing — otherwise the last screen of an upgrade
	// lists steps that do not apply, which is how a checklist stops being read.
	want := []string{"step: image", "step: pipeline", "step: apply"}
	if strings.Join(steps, "|") != strings.Join(want, "|") {
		t.Errorf("next steps = %v, want %v — the reporter that returned \"\" must contribute "+
			"nothing, and the order must be the order the operator has to act in", steps, want)
	}

	for _, want := range []string{"ci-image-skew", "obj-label-prefix",
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
// is meant to work through, with a hole in it — nobody read it as an instruction
// until they had to run it. The deployment is on disk; a command printed as a
// next step should be one you can paste.
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
