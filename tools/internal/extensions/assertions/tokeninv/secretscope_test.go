package tokeninv

// secretscope_test.go — the inventory must have ASKED before it reports absence.
//
// gatherSecretAges walks two scopes, the deployment's GitHub environment and the
// repository. With no environment they are the same request, so the second is
// skipped — and the skip was written by VALUE (`scope == "" && env == ""`), a
// condition true of both iterations when env is "". The probe was never called.
//
// Nothing errored. Ten secrets came back `absent`, which is a verdict and not a
// failure, so SecretProbeVerdict stayed ok and the reconciler published
// llz_credential_configured=0 for every one of them — LLZCredentialUnconfigured
// firing fleet-wide off an inventory that had asked nothing.
//
// Every pre-existing test passes env="infra-primary". This one is parameterised
// over the scope precisely because the defect lived in the value none of them
// used.

import (
	"errors"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/credtargets"
)

// THE PROBE IS CALLED, WHATEVER THE SCOPE. Counting calls rather than reading
// verdicts, because "reported absent" and "never asked" produce the same entry
// and only the call count tells them apart.
func TestEveryScopeActuallyProbes(t *testing.T) {
	for _, env := range []string{"infra-primary", ""} {
		name := env
		if name == "" {
			name = "repo scope (REGION unset)"
		}
		t.Run(name, func(t *testing.T) {
			var calls int
			var sawScopes []string
			got := gatherSecretAges(env, func(scope, _ string) (string, bool, error) {
				calls++
				sawScopes = append(sawScopes, scope)
				return "", false, nil // a genuine 404: the secret is not set
			})
			if len(got) != len(credtargets.GHSecretTargets) {
				t.Fatalf("entries = %d, want one per target (%d)", len(got), len(credtargets.GHSecretTargets))
			}
			if calls == 0 {
				t.Fatal("the probe was never called — every entry below is a claim about a request that was not made")
			}
			// One probe per target at minimum: the loop may try both scopes, but
			// it must try at least one for each.
			if calls < len(credtargets.GHSecretTargets) {
				t.Errorf("probe called %d time(s) for %d target(s) — some target was never asked about",
					calls, len(credtargets.GHSecretTargets))
			}
			// With no environment the repo scope is asked exactly once per target,
			// not twice: the dedupe still has to work.
			if env == "" && calls != len(credtargets.GHSecretTargets) {
				t.Errorf("repo scope probed %d time(s) for %d target(s) — the dedupe stopped deduping",
					calls, len(credtargets.GHSecretTargets))
			}
			for _, s := range sawScopes {
				if env != "" && s != env && s != "" {
					t.Errorf("probed an unexpected scope %q", s)
				}
			}
			// A real 404 IS absent, and must stay reportable as such — the fix
			// must not turn every unset secret into "unknown".
			for _, e := range got {
				if e.State != credtargets.TokenStateAbsent {
					t.Errorf("%s: state = %q, want absent for a genuine 404", e.Name, e.State)
				}
			}
		})
	}
}

// AT THE CONSUMER. The entries feed SecretProbeVerdict, which is what decides
// whether the fleet alert is allowed to trust them. An inventory that could not
// read a scope must make the verdict UNAVAILABLE, so the alert stays quiet
// instead of accusing every credential.
func TestARefusedScopeMakesTheVerdictUnavailableRatherThanOK(t *testing.T) {
	refused := gatherSecretAges("", func(string, string) (string, bool, error) {
		return "", false, errors.New("403 from the GitHub API")
	})
	for _, e := range refused {
		if e.State != credtargets.TokenStateUnknown {
			t.Errorf("%s: state = %q, want unknown when the API refused", e.Name, e.State)
		}
	}
	if got := credtargets.SecretProbeVerdict(true, refused); got != credtargets.SecretProbeUnavailable {
		t.Errorf("verdict = %q, want %q — an unreadable inventory must not read as a clean one",
			got, credtargets.SecretProbeUnavailable)
	}

	// And the inverse, so the gate cannot pass by making everything unavailable:
	// a scope that answered gives an OK verdict.
	answered := gatherSecretAges("", func(string, string) (string, bool, error) {
		return "2026-08-01T00:00:00Z", true, nil
	})
	if got := credtargets.SecretProbeVerdict(true, answered); got != credtargets.SecretProbeOK {
		t.Errorf("verdict = %q, want %q for a fully-answered inventory", got, credtargets.SecretProbeOK)
	}
}
