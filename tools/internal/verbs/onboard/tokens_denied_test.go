package onboard

// tokens_denied_test.go — `llz tokens` must stop on a denied credential, not
// just print one.
//
// THE VERB HAS DONE THIS BEFORE, WITH THE OTHER VERDICT. The nothing-missing
// branch used to print the ✓ and return nil however the VALIDITY probe went, so
// a revoked token produced a warning, a green "everything is set", and "Next
// steps: llz build" — three outputs, the last two contradicting the first.
// InvalidCredentialsError ended that. The scope probe then arrived counting
// nothing on the same path and reproduced it exactly, one verdict later: a PAT
// rendering "PERMS ✗ DENIED" two lines above still reached the green line and
// exit 0, while `llz doctor` on the same instance refused. These pin BOTH
// refusals to the one branch so the next verdict added here cannot repeat it a
// third time.

import (
	"strings"
	"testing"
)

func TestDeniedCredentialsError(t *testing.T) {
	if err := DeniedCredentialsError(0, "acme/platform"); err != nil {
		t.Errorf("no denials must not error; got %v", err)
	}
	err := DeniedCredentialsError(1, "acme/platform")
	if err == nil {
		t.Fatal("a denied credential must produce an error — printing it and exiting 0 is the bug")
	}
	// The remedy is the OPPOSITE of the invalid case, and saying the wrong one
	// costs an afternoon: rotating a live, in-date, under-scoped PAT produces an
	// identically under-scoped replacement.
	if !strings.Contains(err.Error(), "RE-SCOPE") {
		t.Errorf("the error must say re-scope; got %q", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "rotate them") {
		t.Errorf("the error must not read as an instruction to rotate; got %q", err)
	}
	if !strings.Contains(err.Error(), "acme/platform") {
		t.Errorf("the error must name the repo; got %q", err)
	}
}

// THE ONE RULE BOTH VERBS APPLY. `llz tokens` and `llz doctor` route their probe
// results through CredentialRefusal, so the question "does this stop the
// operator here?" has a single answer and a single test — which is what the
// InvalidCredentialsError comment asked for and what adding the scope verdict
// promptly re-broke by wiring only doctor.
func TestCredentialRefusal(t *testing.T) {
	if err := CredentialRefusal(0, 0, "acme/platform"); err != nil {
		t.Errorf("a clean probe must not refuse; got %v", err)
	}
	// A denial alone stops the run. This is the case `llz tokens` was letting
	// through: valid, in date, present, and unable to do its job.
	err := CredentialRefusal(0, 1, "acme/platform")
	if err == nil {
		t.Fatal("a denied credential alone must stop the run — printing it and exiting 0 is the bug")
	}
	if !strings.Contains(err.Error(), "RE-SCOPE") {
		t.Errorf("a denial must lead with re-scope; got %q", err)
	}
	// An invalid one alone stops it too.
	if err := CredentialRefusal(1, 0, "acme/platform"); err == nil || !strings.Contains(err.Error(), "rotate") {
		t.Errorf("an invalid credential must stop the run with rotate; got %v", err)
	}
	// BOTH: invalidity leads, because a dead token cannot be meaningfully
	// scope-probed and re-scoping one that is about to be replaced is wasted work.
	both := CredentialRefusal(1, 1, "acme/platform")
	if both == nil {
		t.Fatal("both faults present must stop the run")
	}
	if !strings.Contains(both.Error(), "rotate") {
		t.Errorf("with both present the rotate instruction must lead; got %q", both)
	}
}

// THE TWO REFUSALS MUST NOT BE INTERCHANGEABLE. One function returning both
// messages, or two returning the same one, is how an operator gets told to
// rotate a credential whose only problem is its scope.
func TestInvalidAndDeniedGiveOppositeRemedies(t *testing.T) {
	invalid := InvalidCredentialsError(1, "acme/platform").Error()
	denied := DeniedCredentialsError(1, "acme/platform").Error()
	if invalid == denied {
		t.Fatal("the two refusals must not render identically")
	}
	if !strings.Contains(invalid, "rotate") {
		t.Errorf("the invalid-credential refusal must say rotate; got %q", invalid)
	}
	if !strings.Contains(denied, "RE-SCOPE") {
		t.Errorf("the denied-credential refusal must say re-scope; got %q", denied)
	}
}
