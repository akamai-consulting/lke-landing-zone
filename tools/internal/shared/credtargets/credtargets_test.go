package credtargets

// Like credpaths, this table left its old package with no tests of its own after
// four peers had been reading it. What is worth checking is the invariants every
// consumer assumes and none of them states.

import (
	"strings"
	"testing"
)

func TestEveryTargetCarriesAKnownExpectation(t *testing.T) {
	known := map[string]bool{
		CredExpectPresent: true, CredExpectOptional: true, CredExpectAbsent: true,
	}
	for _, tt := range GHSecretTargets {
		if !known[tt.Expect] {
			// `absent` is the one that matters: it means the API ANSWERED 404, which
			// is a PASS for a credential meant to be revoked and a FAILURE for one
			// meant to exist. An unknown value falls out of every consumer's switch
			// and is silently treated as neither.
			t.Errorf("%s has expectation %q, which is not one of the three", tt.Name, tt.Expect)
		}
		if tt.Name == "" {
			t.Error("a target with no name cannot be probed or reported")
		}
	}
	if len(GHSecretTargets) == 0 {
		t.Fatal("no secret targets — every consumer would report a clean inventory")
	}
}

func TestTargetNamesAreUnique(t *testing.T) {
	// Consumers key maps by Name; a duplicate silently drops one target from the
	// inventory, and a credential nobody looks at is exactly the failure this
	// table exists to prevent.
	seen := map[string]bool{}
	for _, tt := range GHSecretTargets {
		if seen[tt.Name] {
			t.Errorf("duplicate secret target %q", tt.Name)
		}
		seen[tt.Name] = true
	}
	seenPAT := map[string]bool{}
	for _, tt := range GHPATTargets {
		if seenPAT[tt.Name] {
			t.Errorf("duplicate PAT target %q", tt.Name)
		}
		seenPAT[tt.Name] = true
		// Name is both the env var read at probe time and the `token` label on the
		// metric, so an empty one produces an unlabelled series nobody can alert on.
		if tt.Name == "" || strings.ToUpper(tt.Name) != tt.Name {
			t.Errorf("PAT target %q should be an env-style (upper-case) name", tt.Name)
		}
	}
}

// SecretProbeVerdict decides whether the whole secret-age lane is trustworthy.
// Reporting `ok` when the client never got built would publish a clean age report
// derived from nothing.
func TestSecretProbeVerdict(t *testing.T) {
	if got := SecretProbeVerdict(false, nil); got != SecretProbeUnavailable {
		t.Errorf("no client built → %q, want %q", got, SecretProbeUnavailable)
	}
	if got := SecretProbeVerdict(true, []SecretEntry{{Name: "X", State: TokenStateOK}}); got != SecretProbeOK {
		t.Errorf("client built with entries → %q, want %q", got, SecretProbeOK)
	}
}
