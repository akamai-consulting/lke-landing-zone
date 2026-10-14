package onboard

import "testing"

// TestTokensCommandCarriesNoPlaceholder.
//
// This command leads the checklist `llz upgrade` prints, and it is the first
// thing an operator is asked to run after an upgrade. It carried `<env>` in the
// ci-image line while the missing-items line four rows below resolved the
// deployment — one report, two spellings, and the unresolved one at the top.
func TestTokensCommandCarriesNoPlaceholder(t *testing.T) {
	got := tokensCommand("prod", false)
	if got != "llz tokens --env prod --yes" {
		t.Errorf("tokensCommand = %q, want the deployment named and pasteable", got)
	}
	if admin := tokensCommand("e2e", true); admin == got {
		t.Errorf("the admin form must differ from the ordinary one; both were %q", admin)
	}
}
