package copier

import (
	"strings"
	"testing"
)

// TestMissingCopierErrorAppendsTheCIRemedy pins the shape a substitution got
// wrong, over the PURE function so it runs everywhere.
//
// The first cut REPLACED the whole message when GITHUB_ACTIONS was set. CI sets
// that variable, so two existing tests asserting the install routes got the CI
// text and failed there while passing on a laptop, where it is unset — a branch
// keyed on the one thing that distinguishes CI from local is invisible locally by
// construction. The second cut tested it through Require(), which short-circuits
// when copier IS installed, so it skipped on every developer machine and verified
// nothing.
func TestMissingCopierErrorAppendsTheCIRemedy(t *testing.T) {
	local := missingCopierError("`llz new`", false).Error()
	ci := missingCopierError("`llz new`", true).Error()

	// The install routes are what a local operator can act on, and OTHER packages'
	// tests assert this text — so CI must keep it too.
	for _, s := range []string{local, ci} {
		if !strings.Contains(s, "pipx install copier") {
			t.Error("the install routes must survive in both shapes; existing callers assert them")
		}
		if !strings.Contains(s, "llz doctor") {
			t.Error("the toolchain pointer must survive in both shapes")
		}
	}
	// A laptop has no TF_IMAGE to bump; that advice is noise there.
	if strings.Contains(local, "TF_IMAGE") {
		t.Error("the CI remedy must not appear outside CI")
	}
	// Inside the pinned container `pipx install` is unactionable — the stale image
	// variable is the real cause, and nothing else names it.
	if !strings.Contains(ci, "gh variable set TF_IMAGE") {
		t.Error("in CI the diagnostic must name the stale image variable")
	}
	if len(ci) <= len(local) {
		t.Error("the CI remedy is APPENDED, so the CI message must be strictly longer")
	}
}
