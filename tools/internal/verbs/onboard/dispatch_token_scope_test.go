package onboard

// dispatch_token_scope_test.go — the fine-grained PAT this wizard MINTS must
// carry every permission the e2e run actually uses.
//
// THE SPLIT CONTRACT THAT BIT. `llz ci assert-instance-pr-gates` needs
// pull-requests:write; the release-e2e workflows were updated to SAY so, and
// ghFineGrainedDispatchURL — the only code that actually pre-fills the token
// creation page — was not. A token minted from the wizard would have passed the
// presence check and then died ~10 minutes into the run at `gh pr create` with
// "Resource not accessible by integration": the exact failure the documented
// scope change existed to prevent. Prose and a URL builder both state this
// contract; only one of them is executable, so the test holds them together.

import (
	"net/url"
	"strings"
	"testing"
)

func TestDispatchTokenURLCarriesEveryPermissionTheE2EUses(t *testing.T) {
	raw := ghFineGrainedDispatchURL("llz-e2e-dispatch", "acme")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("ghFineGrainedDispatchURL produced an unparseable URL %q: %v", raw, err)
	}
	q := u.Query()
	for perm, why := range map[string]string{
		"contents":      "force-pushes the instantiated tree",
		"actions":       "dispatches and watches the instance's workflows",
		"workflows":     "the force-push rewrites .github/workflows/*",
		"pull_requests": "assert-instance-pr-gates opens the throwaway PR that proves the instance's PR-gated CI runs",
	} {
		if got := q.Get(perm); got != "write" {
			t.Errorf("the minted token requests %s=%q, want \"write\" — %s. A token created from this "+
				"URL passes the presence check and then fails at the step that needs the permission.",
				perm, got, why)
		}
	}
	if q.Get("name") == "" || q.Get("target_name") != "acme" || q.Get("expires_in") == "" {
		t.Errorf("the pre-filled name/owner/expiry are incomplete: %v", q)
	}
}

// And the sentence the operator reads has to name the same set — they act on the
// prose, not on the query string.
func TestDispatchTokenPromptNamesPullRequests(t *testing.T) {
	// The prompt is printed by tokens.go; assert on the literal it shares with
	// the URL builder so the two cannot drift apart silently.
	const wantPerm = "Pull requests"
	src := tokensPromptFineGrained
	if !strings.Contains(src, wantPerm) {
		t.Errorf("the fine-grained token instruction does not mention %q, so an operator following it "+
			"produces a token missing the permission the PR-gate probe needs: %q", wantPerm, src)
	}
	for _, p := range []string{"Contents", "Actions", "Workflows"} {
		if !strings.Contains(src, p) {
			t.Errorf("the fine-grained token instruction dropped %q: %q", p, src)
		}
	}
}
