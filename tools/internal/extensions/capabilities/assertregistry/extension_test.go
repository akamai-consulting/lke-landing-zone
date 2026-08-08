package assertregistry

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("assert-registry does not validate: %v", err)
		}
	}
}

// THE GRANT IS THE SCAR. This lane exists because managed instances once rendered
// HARBOR_HOST as "harbor." — non-empty, so it defeated every empty-string guard —
// and every push and pull 401'd while every credential in the chain was valid.
// Nothing caught it because nothing ever USED the credential: the provisioner
// asserted it had CREATED a robot, not that the robot could log in.
//
// So secret-read is load-bearing, not decorative. An assertion that only read
// metadata could not have caught it; this one has to hold the credential and try.
func TestHarborRoundTripHoldsTheCredentialItTests(t *testing.T) {
	b := Extension().Bindings[0]
	if !hasGrant(b, extension.SecretRead) {
		t.Error("dropped secret-read — this lane must USE the robot credential, not merely " +
			"observe that one exists. The bug it was written for is exactly the gap between those two")
	}
	if hasGrant(b, extension.SecretCustody) {
		t.Error("secret-custody claims this PLACES credential material; it reads a Secret and " +
			"logs in with it. That is the distinction token-inventory forced into the vocabulary")
	}
}

// Opt-in: an instance without Harbor has no robot to exercise, and a lane that
// always fails on such an instance trains operators to ignore lanes.
func TestAssertRegistryIsOptIn(t *testing.T) {
	if Extension().Always {
		t.Error("Always = true — an instance without Harbor has nothing here to assert about")
	}
}

// The second capability/assertion pair to land half of. assert-reconciler settled
// that such pairs should NOT merge; this is the cleaner illustration, so pin the
// read-only half: harbor-provisioner will hold cloud-mutate and secret-custody to
// MINT the robot, and nothing in a merged union would be true of either side.
func TestAssertionHalfStaysReadOnly(t *testing.T) {
	for _, g := range Extension().Bindings[0].Grants {
		switch g {
		case extension.ClusterRead, extension.SecretRead:
		default:
			t.Errorf("holds %q — the assertion half of a provisioner/assertion pair must stay "+
				"read-only, or merging it with harbor-provisioner becomes indistinguishable "+
				"from leaving them separate", g)
		}
	}
}

func hasGrant(b extension.Binding, want extension.Grant) bool {
	for _, g := range b.Grants {
		if g == want {
			return true
		}
	}
	return false
}
