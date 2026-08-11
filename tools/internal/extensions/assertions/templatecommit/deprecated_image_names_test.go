package templatecommit

// deprecated_image_names_test.go — a pin carrying a RETIRED image name is still
// ours to re-pin, and must still be reported.
//
// The gap this pins: ci-terraform was renamed ci-tofu (ADR 0008) and published
// as an alias so existing pins keep resolving. ComputedImageRef matched only the
// current name, so `StaleCIImageVars` — the single chokepoint under `llz
// upgrade`, `llz doctor` AND `llz tokens` — classified a `ci-terraform` pin as
// somebody else's image and skipped it. The rename made the rename invisible to
// the check that exists to catch it, in the direction that matters: the aliased
// image has no `tofu` binary, and every delivered Terraform job runs `tofu`.

import "testing"

func TestComputedImageRefAcceptsDeprecatedNames(t *testing.T) {
	// The exact value observed downstream, and the reason its instance's CI
	// could not run: reported by nothing, re-pinned by nothing.
	if !ComputedImageRef("ghcr.io/akamai-consulting/ci-terraform:1.9.8", "ci-tofu") {
		t.Error("a ci-terraform pin is not recognised as ours to re-pin — " +
			"upgrade/doctor/tokens will all skip it while its image lacks tofu")
	}
	if !ComputedImageRef("ghcr.io/akamai-consulting/ci-tofu:sha-abc123", "ci-tofu") {
		t.Error("the current name must keep matching")
	}

	// Pin the exclusions: the alias must not become a licence to re-pin images
	// that were never ours. An operator's own registry, org or image still wins.
	for _, ref := range []string{
		"ghcr.io/someone-else/ci-terraform:1.9.8",
		"docker.io/akamai-consulting/ci-terraform:1.9.8",
		"ghcr.io/akamai-consulting/ci-kubernetes:1.31.0",
		"ghcr.io/akamai-consulting/ci-terraform", // no tag: nothing to re-pin
	} {
		if ComputedImageRef(ref, "ci-tofu") {
			t.Errorf("%q was claimed as ours to re-pin", ref)
		}
	}
}

// TestDeprecatedNamesAreNotSelfReferential keeps the table honest: listing an
// image under its own name would make the loop above quietly redundant.
func TestDeprecatedNamesAreNotSelfReferential(t *testing.T) {
	for current, olds := range deprecatedImageNames {
		for _, o := range olds {
			if o == current {
				t.Errorf("%q lists itself as a deprecated name", current)
			}
		}
	}
}
