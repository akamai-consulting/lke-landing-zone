package tokeninv

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("token-inventory does not validate: %v", err)
		}
	}
}

// `configured` was the last state in the vocabulary no extension had claimed.
// Pin it: if these bindings drift later, the state goes back to being decorative,
// and a state nothing binds is indistinguishable from one that should not exist.
func TestTokenInventoryIsTheFirstConfiguredBinding(t *testing.T) {
	var atConfigured int
	for _, b := range Extension().Bindings {
		if b.State == extension.Configured {
			atConfigured++
		}
	}
	if atConfigured != 2 {
		t.Errorf("want two bindings at configured (validate-tokens, rotation-plan), got %d — "+
			"these checks run before anything provisions, which is what the state means", atConfigured)
	}
}

// THE REGRESSION THIS GUARDS is the one that forced the grant split.
//
// validate-tokens makes network calls to GitHub, Linode and S3. Declaring it a
// GATE is the tempting mistake — it blocks the pipeline, which is what gates do —
// but a gate here is defined as the fast pre-commit path over files alone, and it
// may hold read-repo and nothing else. It is an assertion.
//
// It also may not hold secret-custody: it READS credentials and mutates nothing.
// Before the split there was no word for that, and this declaration was
// impossible to write honestly.
func TestValidateTokensIsAnAssertionThatOnlyReadsCredentials(t *testing.T) {
	b, ok := bindingNamed(t, "validate-tokens")
	if !ok {
		return
	}
	if b.Kind != extension.Assertion {
		t.Errorf("kind = %s, want assertion — it probes GitHub/Linode/S3 over the network, "+
			"so it is not a gate however much it behaves like one", b.Kind)
	}
	for _, g := range b.Grants {
		if g == extension.SecretCustody {
			t.Error("secret-custody claims this PLACES credential material; it only reads it. " +
				"That conflation is exactly what the secret-read split removed")
		}
	}
	if !hasGrant(b, extension.SecretRead) {
		t.Error("dropped secret-read — it still reads every pipeline credential out of the " +
			"environment, so dropping the grant makes the declaration the thing that is wrong")
	}
}

// The expiry inventory emits metadata only — "never a token value", per the
// command's own help. secret-read is therefore the ceiling as well as the floor.
func TestTokenInventoryLaneNeverClaimsCustody(t *testing.T) {
	b, ok := bindingNamed(t, "token-inventory")
	if !ok {
		return
	}
	if b.Kind != extension.Invariant || b.State != extension.Operating {
		t.Errorf("binding = %s, want invariant:operating — it is scheduled against a working "+
			"cluster and its failure means a credential DRIFTED toward expiry", b)
	}
	if hasGrant(b, extension.SecretCustody) {
		t.Error("secret-custody on the inventory would mean it writes credential material; " +
			"it emits a ConfigMap of metadata and never a token value")
	}
}

func bindingNamed(t *testing.T, name string) (extension.Binding, bool) {
	t.Helper()
	for _, b := range Extension().Bindings {
		if b.Name == name {
			return b, true
		}
	}
	t.Errorf("no binding named %q", name)
	return extension.Binding{}, false
}

func hasGrant(b extension.Binding, want extension.Grant) bool {
	for _, g := range b.Grants {
		if g == want {
			return true
		}
	}
	return false
}
