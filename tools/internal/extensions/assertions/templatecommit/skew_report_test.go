package templatecommit

import (
	"strings"
	"testing"
)

// The gate on the distinction this file exists to make: "both variables already
// name this pin" and "there was no pinned answer to compare them to" must not
// arrive as the same value.
//
// Collapsing them is not an abstract tidiness point — it is what left an
// upgrade's most time-critical instruction off its own checklist. `llz upgrade`
// returned early on the empty slice and printed no re-pin step; `llz doctor`
// printed a green "TF_IMAGE / KUBE_IMAGE match the template pin" for a comparison
// that never ran. Both were reading an abstention as agreement.
//
// THE UNPUBLISHED-IMAGE PATH IS THE ONE UNDER TEST, deliberately: it is the state
// every instance is in during the minutes after a release, while build-images.yml
// is still publishing the images for the commit the new pin names — which is
// exactly when someone runs `llz upgrade`. A 40-hex ref needs no round-trip to
// resolve, so this stays offline and deterministic.
func TestCIImageSkewReportSaysWhenItCouldNotCompare(t *testing.T) {
	const sha = "b9fe2721b55e2cb196d418f8d0bc6069957e3bd3"
	recorded := func(k string) string {
		return map[string]string{
			"TF_IMAGE":   CIImageRef("akamai-consulting", "ci-tofu", "sha-0000000000000000000000000000000000000000"),
			"KUBE_IMAGE": CIImageRef("akamai-consulting", "ci-kubernetes", "sha-0000000000000000000000000000000000000000"),
		}[k]
	}

	t.Run("no pinned answer reports why", func(t *testing.T) {
		stubImagePublished(t, func(string) (bool, bool) { return false, true })

		skew, unchecked := CIImageSkewReport(sha, recorded)
		if unchecked == "" {
			t.Fatal("an unpublished pin reported no reason — the caller cannot tell this from " +
				"\"both variables are already correct\", which is how the re-pin step went missing")
		}
		if !strings.Contains(unchecked, "never published") {
			t.Errorf("unchecked = %q, want it to name WHICH refusal happened — an unresolvable "+
				"ref and an unpublished image need different remedies", unchecked)
		}
		if len(skew) != 0 {
			t.Errorf("skew = %v, want none: with no commit-pinned answer the only thing to re-pin "+
				"onto is the floating tags, and advising that would make a pinned instance worse", skew)
		}
	})

	// The wrapper must keep its old shape. `llz tokens` re-pins from it and writes
	// what it returns, so an abstention has to stay an empty slice there — the
	// value it would otherwise write is the floating tag.
	t.Run("StaleCIImageVars keeps abstaining", func(t *testing.T) {
		stubImagePublished(t, func(string) (bool, bool) { return false, true })
		if got := StaleCIImageVars(sha, recorded); len(got) != 0 {
			t.Errorf("StaleCIImageVars = %v, want none", got)
		}
	})

	// Nothing recorded is a real "nothing to say", not an abstention: filling the
	// variables belongs to ComputeAndReportImageVars, and a fresh instance has not
	// got there yet. Reporting it as unchecked would put a step on every first-run
	// checklist that does not apply.
	t.Run("nothing recorded is silent", func(t *testing.T) {
		stubImagePublished(t, func(string) (bool, bool) { return false, true })
		skew, unchecked := CIImageSkewReport(sha, func(string) string { return "" })
		if len(skew) != 0 || unchecked != "" {
			t.Errorf("skew=%v unchecked=%q, want both empty", skew, unchecked)
		}
	})
}
