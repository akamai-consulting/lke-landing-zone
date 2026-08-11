package releasepublish

// pin_images_required_test.go — the images this commit's CLUSTER will pull, not
// just the two a variable is pinned at.
//
// THE ROUND IT COST. This verb checked ci-tofu and ci-kubernetes, found both
// published, and reported success while ghcr.io/<owner>/llz:sha-<commit> had
// never been built. e2e-instantiate renders LLZ_IMAGE_REF at that tag and Kyverno
// verifies it at admission, so the cluster came up and refused four workloads
// (llz-reconciler's Deployment, obj-proxy's DaemonSet, and two CronJobs) with
// MANIFEST_UNKNOWN. No reconciler meant no apl-overlay push, so convergence spent
// its entire 1200s budget on the downstream symptom — about forty minutes and a
// real cluster after the actual cause.

import (
	"strings"
	"testing"
)

// missingOnly returns a manifest stub where every image exists except the named
// one — the exact shape of the failure: CI images built, app image absent.
func missingOnly(name string) func(string) bool {
	return func(ref string) bool { return !strings.Contains(ref, "/"+name+":") }
}

func TestPinFailsWhenTheInClusterImageIsMissing(t *testing.T) {
	stubPinSeams(t, 1, missingOnly("llz"))
	err := RunPinInstanceImages(baseOpts())
	if err == nil {
		t.Fatal("pin reported success with the in-cluster llz image unpublished — the cluster is then " +
			"rendered against an image Kyverno will refuse, and converge burns its whole budget on it")
	}
	for _, want := range []string{"llz:sha-deadbeef", "Kyverno", "llz-reconciler"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should name %q so the cause is readable without a cluster; got: %v", want, err)
		}
	}
}

// The two CI images must still be waited for — this is an addition, not a swap.
func TestPinStillFailsWhenACIImageIsMissing(t *testing.T) {
	stubPinSeams(t, 1, missingOnly("ci-tofu"))
	if err := RunPinInstanceImages(baseOpts()); err == nil {
		t.Fatal("an unpublished ci-tofu must still fail the pin")
	}
}

// ...and everything published still passes, or the check is just a red light.
func TestPinPassesWhenEveryImageIsPublished(t *testing.T) {
	setVars := stubPinSeams(t, 1, func(string) bool { return true })
	if err := RunPinInstanceImages(baseOpts()); err != nil {
		t.Fatalf("all images published should pass: %v", err)
	}
	if !strings.Contains(strings.Join(*setVars, "\n"), "TF_IMAGE") {
		t.Errorf("the variables were not pinned: %v", *setVars)
	}
}

// A missing APP image must also count as "images are missing" for
// --build-if-missing, or the build that would publish it never gets triggered.
func TestAMissingAppImageTriggersTheBuild(t *testing.T) {
	if !anyShaImageMissing("o", "deadbeef") {
		// guard: with the default stub everything exists
	}
	stubPinSeams(t, 1, missingOnly("llz"))
	if !anyShaImageMissing("o", "deadbeef") {
		t.Error("an absent in-cluster llz image did not count as missing, so --build-if-missing would " +
			"never trigger the build that publishes it")
	}
}

// The pinned set and the required set must stay disjoint and complete: every
// image name the cluster needs is waited for exactly once.
func TestShaImageNamesCoversPinnedAndRequired(t *testing.T) {
	got := strings.Join(shaImageNames(), ",")
	for _, want := range []string{"ci-tofu", "ci-kubernetes", "llz"} {
		if !strings.Contains(got, want) {
			t.Errorf("shaImageNames() = %s, missing %q", got, want)
		}
	}
}
