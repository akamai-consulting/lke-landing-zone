package tokeninv

// validate_optionality_test.go — one fact, one source: whether a credential
// blocks the run.
//
// `llz doctor` reads it off envreq.E2ERequirements' Required column. This
// preflight used to keep its own two-name map of the same fact, and the two
// agreed only because nobody had yet changed one — while comments in both places
// claimed doctor and CI "agree on what stops a build". These call BOTH sides'
// real functions rather than restating the rule, which is the only version of
// this test that cannot pass while the shipped consumers disagree.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envreq"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tokenprobe"
)

// FAIL CLOSED ON A NAME THE TABLE DOES NOT CARRY. optional() treats an unknown
// credential as REQUIRED, which is the safe direction — but a probed credential
// that fell out of the table would then block with no row explaining why, and one
// that was never in it has no owner at all. Assert the two sets line up so the
// fallback stays a guard rather than a routine path.
func TestEveryValidatableTokenIsInTheRequirementTable(t *testing.T) {
	inTable := map[string]bool{}
	for _, r := range envreq.E2ERequirements(true) {
		inTable[r.Name] = true
	}
	for _, name := range validatableTokens {
		if !inTable[name] {
			t.Errorf("%s is probed by validate-tokens but absent from envreq.E2ERequirements — "+
				"it would block on the fail-closed default with no table row to explain it", name)
		}
	}
}

// THE VERDICT ITSELF MATCHES, credential by credential. Reading the table is the
// mechanism; this is the property that mechanism exists to hold.
func TestOptionalityMatchesTheRequirementTable(t *testing.T) {
	for _, r := range envreq.E2ERequirements(true) {
		if tokenprobe.KindFor(r.Name) == tokenprobe.KindNone {
			continue // not a probeable credential
		}
		if got, want := optional(r.Name), !r.Required; got != want {
			t.Errorf("%s: validate-tokens optional=%v, requirement table Required=%v — "+
				"doctor and CI would disagree about whether this stops a build", r.Name, got, r.Required)
		}
	}
}

// And the fallback is REQUIRED, not optional: an unknown credential must never
// be waved through.
func TestUnknownCredentialIsTreatedAsRequired(t *testing.T) {
	if optional("SOME_CREDENTIAL_THE_TABLE_NEVER_HEARD_OF") {
		t.Error("an unknown credential must fail closed (required), not be treated as optional")
	}
}
