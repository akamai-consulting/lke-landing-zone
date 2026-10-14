package tokenprobe

import (
	"strings"
	"testing"
)

// testCapCtx builds the probe context the way `llz ci validate-tokens` does, so
// the tests that set GH_REPO/REGION keep driving the same code path CI drives.
func testCapCtx() CapContext { return EnvCapContext() }

// The two OPENBAO_SECRETS_WRITE_TOKEN checks, named so assertions say which
// permission they are about. Both are on ONE credential and they are not
// interchangeable — that is the whole reason CheckCapabilities returns a slice.
const (
	opEnvSecrets  = "write infra-<region> environment secrets"
	opRepoSecrets = "read/write REPO-level Actions secrets"
)

// TestClassifyCapabilityStatus pins the verdict table, and specifically the two
// codes that must NOT be conflated: 403 (authenticated but under-scoped → block)
// versus 404 (target absent OR invisible → ambiguous, warn only). Blocking on
// 404 would trade a late true positive for an early false one.
func TestClassifyCapabilityStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		want CapabilityStatus
	}{
		{"authorized", 200, CapOK},
		{"no content", 204, CapOK},
		{"under-scoped", 403, CapDenied},
		{"rejected", 401, CapDenied},
		{"ambiguous", 404, CapUnknown},
		{"unreachable", 0, CapUnknown},
		{"server error", 500, CapUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := classifyCapabilityStatus(tc.code, "do the thing", capREST)
			if got != tc.want {
				t.Errorf("code %d: status %v, want %v", tc.code, got, tc.want)
			}
			if got != CapOK && detail == "" {
				t.Errorf("code %d: want a non-empty detail explaining the verdict", tc.code)
			}
		})
	}

	// A denial must say the token is under-scoped, NOT that it expired — the
	// remediation is re-scoping, and "rotate it" sends the operator the wrong way.
	_, detail := classifyCapabilityStatus(403, "write environment secrets", capREST)
	if !strings.Contains(detail, "under-scoped") {
		t.Errorf("403 detail = %q, want it to name the scope as the cause", detail)
	}
}

// TestProbeCapability_SkipsWithoutContext verifies that a missing GH_REPO/REGION
// skips rather than fails. The probe can't be built without them, and that is
// never the token's fault.
func TestProbeCapability_SkipsWithoutContext(t *testing.T) {
	orig := GHCapabilityProbe
	t.Cleanup(func() { GHCapabilityProbe = orig })
	called := false
	GHCapabilityProbe = func(_, _, _ string) (int, error) { called = true; return 200, nil }

	t.Setenv("GH_REPO", "")
	t.Setenv("REGION", "")
	cr := probeCapability(testCapCtx(), capCheckFor(t, "OPENBAO_SECRETS_WRITE_TOKEN", opEnvSecrets), "tok")
	if cr.Status != CapSkipped {
		t.Errorf("status = %v, want CapSkipped", cr.Status)
	}
	if called {
		t.Error("probed the API without the context to build a path")
	}
}

// TestProbeCapability_ProbesTheRealEndpoint asserts the seal-key check hits the
// exact path `gh secret set --env infra-<region>` fetches. If this drifts, the
// check stops being the read-only twin of the real call and starts guessing.
func TestProbeCapability_ProbesTheRealEndpoint(t *testing.T) {
	orig := GHCapabilityProbe
	t.Cleanup(func() { GHCapabilityProbe = orig })
	var gotPath string
	GHCapabilityProbe = func(_, _, path string) (int, error) { gotPath = path; return 403, nil }

	t.Setenv("GH_REPO", "acme/platform")
	t.Setenv("REGION", "prod")
	cr := probeCapability(testCapCtx(), capCheckFor(t, "OPENBAO_SECRETS_WRITE_TOKEN", opEnvSecrets), "tok")

	const want = "/repos/acme/platform/environments/infra-prod/secrets/public-key"
	if gotPath != want {
		t.Errorf("probed %q, want %q", gotPath, want)
	}
	if cr.Status != CapDenied {
		t.Errorf("403 → status %v, want CapDenied", cr.Status)
	}
}

// TestCheckCapability_OnlyRegisteredTokens confirms credentials with no scope
// requirement report nothing (rather than a bogus verdict).
func TestCheckCapability_OnlyRegisteredTokens(t *testing.T) {
	orig := GHCapabilityProbe
	t.Cleanup(func() { GHCapabilityProbe = orig })
	GHCapabilityProbe = func(_, _, _ string) (int, error) { return 200, nil }
	t.Setenv("GH_REPO", "acme/platform")
	t.Setenv("REGION", "prod")

	// LINODE_API_TOKEN used to be the example of a credential with NO scope check.
	// It has one now (issue #449) — the version catalog it must be able to read —
	// so the "no check registered" case is carried by a credential that genuinely
	// has none.
	if got := CheckCapabilities(testCapCtx(), "GHCR_READ_TOKEN", "tok"); len(got) != 0 {
		t.Errorf("GHCR_READ_TOKEN has no registered scope check, got %d result(s)", len(got))
	}
	// TWO, not one. Both of this PAT's grants are required by different consumers
	// and a caller that sees only one of them cannot report the other's denial.
	got := CheckCapabilities(testCapCtx(), "OPENBAO_SECRETS_WRITE_TOKEN", "tok")
	if len(got) != 2 {
		t.Fatalf("OPENBAO_SECRETS_WRITE_TOKEN scope checks = %d, want 2 (environment secrets + repo-level secrets)", len(got))
	}
	for _, cr := range got {
		if h := CapabilityHint(cr.Name, cr.Op); h == "" {
			t.Errorf("a denial of %q must carry remediation text", cr.Op)
		}
	}
}

// TestSealKeyHintNamesEnvironmentsPermission guards the remediation text against
// regressing to "Secrets: write". That is the intuitive answer and the wrong
// one: GitHub governs /repos/{o}/{r}/environments/{env}/secrets/* under the
// ENVIRONMENTS permission, while "Secrets" covers only repo-level Actions
// secrets. A PAT with Actions + Secrets: write — exactly what the wizard used to
// mint — authenticates, passes every preflight, and still 403s on the first
// environment-secret write. Pointing an operator at the Secrets toggle sends
// them to a control that changes nothing.
func TestSealKeyHintNamesEnvironmentsPermission(t *testing.T) {
	h := CapabilityHint("OPENBAO_SECRETS_WRITE_TOKEN", opEnvSecrets)
	if !strings.Contains(h, "Environments: write") {
		t.Errorf("hint must name the Environments permission; got %q", h)
	}
	if !strings.Contains(h, "Environment admin") {
		t.Errorf("hint must also name the Environment-admin requirement; got %q", h)
	}
}

// AND IT MUST NOT TELL THE OPERATOR TO WITHHOLD THE OTHER GRANT. This hint read
// "needs Environments: write — NOT \"Secrets: write\"", which was correct while
// Environments was the only permission this PAT needed and became the most
// misleading line the tool could print once the repo-level Secrets check landed:
// the two checks are on ONE credential, so an Environments denial was answered
// with advice that fails the other check. Saying "Secrets does not cover
// environment secrets" is still right and still worth saying; saying it in a way
// that reads as "do not grant Secrets" is not.
func TestSealKeyHintDoesNotTellTheOperatorToWithholdSecrets(t *testing.T) {
	h := CapabilityHint("OPENBAO_SECRETS_WRITE_TOKEN", opEnvSecrets)
	// It must send them to BOTH grants, since this one credential needs both.
	if !strings.Contains(h, "Secrets: write as well") {
		t.Errorf("the hint must say the same PAT also needs Secrets: write; got %q", h)
	}
	if !strings.Contains(h, "Grant BOTH") {
		t.Errorf("the hint must ask for both grants explicitly; got %q", h)
	}
	// The negative form that caused this: a bare "NOT Secrets: write" with no
	// counterweight. Guard the literal, because it is what the sentence collapses
	// back to under a well-meaning edit.
	if strings.Contains(h, "— NOT \"Secrets: write\"") {
		t.Errorf("the hint must not read as an instruction to withhold Secrets: write; got %q", h)
	}
}

// THE OTHER FOUR COPIES ARE OUTSIDE THIS PACKAGE and cannot be reached from
// here, so this asserts what CAN be: that both hints on this one credential point
// at both grants. A reader who lands on either denial must not come away with
// half the permission set.
func TestBothHintsOnThisPATNameBothGrants(t *testing.T) {
	for _, op := range []string{opEnvSecrets, opRepoSecrets} {
		h := CapabilityHint("OPENBAO_SECRETS_WRITE_TOKEN", op)
		if h == "" {
			t.Fatalf("%q has no hint", op)
		}
		if !strings.Contains(h, "Environments: write") {
			t.Errorf("%q hint must name Environments: write; got %q", op, h)
		}
		if !strings.Contains(h, "Secrets") {
			t.Errorf("%q hint must name the Secrets permission; got %q", op, h)
		}
	}
}

// capCheckFor looks a check up by credential name. Tests must not index
// capabilityChecks positionally — the order is presentation, not contract, and a
// new entry would silently repoint an existing assertion at the wrong probe.
func capCheckFor(t *testing.T, name string, ops ...string) capabilityCheck {
	t.Helper()
	var found []capabilityCheck
	for _, c := range capabilityChecks {
		if c.token != name {
			continue
		}
		if len(ops) > 0 && c.op != ops[0] {
			continue
		}
		found = append(found, c)
	}
	switch len(found) {
	case 1:
		return found[0]
	case 0:
		t.Fatalf("no capability check registered for %q %v", name, ops)
	default:
		// A credential with several checks must be addressed by op. Returning the
		// first would quietly point an assertion at a probe it was not written for.
		t.Fatalf("%q has %d checks — pass the op to disambiguate", name, len(found))
	}
	return capabilityCheck{}
}

// TestValuesRepoProbeIsGitRefDiscovery pins the values-repo check to the git
// smart-HTTP ref-discovery request — the exact call whose failure Argo CD
// reports as "failed to list refs: authentication required: Unauthorized". If
// this drifts to a REST path it stops testing the door that actually failed:
// api.github.com and github.com authorize independently, and the scar case is a
// token that passes the former and is refused by the latter.
func TestValuesRepoProbeIsGitRefDiscovery(t *testing.T) {
	origGit, origREST := GitRefsProbe, GHCapabilityProbe
	t.Cleanup(func() { GitRefsProbe, GHCapabilityProbe = origGit, origREST })

	var gotServer, gotPath string
	GitRefsProbe = func(server, _, path string) (int, error) {
		gotServer, gotPath = server, path
		return 200, nil
	}
	GHCapabilityProbe = func(_, _, _ string) (int, error) {
		t.Error("values-repo check must not use the REST transport")
		return 200, nil
	}

	t.Setenv("GH_REPO", "acme/platform")
	t.Setenv("GITHUB_SERVER_URL", "")
	cr := probeCapability(testCapCtx(), capCheckFor(t, "APL_VALUES_REPO_TOKEN"), "tok")

	const wantPath = "/acme/platform.git/info/refs?service=git-upload-pack"
	if gotPath != wantPath {
		t.Errorf("probed %q, want %q", gotPath, wantPath)
	}
	if gotServer != "https://github.com" {
		t.Errorf("server = %q, want the git host (NOT api.github.com)", gotServer)
	}
	if cr.Status != CapOK {
		t.Errorf("200 → status %v, want CapOK", cr.Status)
	}
}

// TestValuesRepoProbeDeniesOnUnauthorized covers the failure this check was
// built for: GitHub answers ref discovery with 401 when the credential is
// refused for the repo. That must BLOCK — it is the same refusal Argo hits ~40
// minutes later, by which point the cluster, apl-core and the Argo bridge are up
// and a human unwinds the mess by hand.
func TestValuesRepoProbeDeniesOnUnauthorized(t *testing.T) {
	orig := GitRefsProbe
	t.Cleanup(func() { GitRefsProbe = orig })
	GitRefsProbe = func(_, _, _ string) (int, error) { return 401, nil }

	t.Setenv("GH_REPO", "acme/platform")
	cr := probeCapability(testCapCtx(), capCheckFor(t, "APL_VALUES_REPO_TOKEN"), "tok")
	if cr.Status != CapDenied {
		t.Fatalf("401 → status %v, want CapDenied", cr.Status)
	}
	// A live-but-refused token needs re-scoping. "Rotate it" is the wrong advice
	// and burns an operator's afternoon minting a replacement with the same gap.
	if strings.Contains(cr.Detail, "rotate the token") {
		t.Errorf("401 detail must not prescribe rotation; got %q", cr.Detail)
	}
}

// TestValuesRepoProbeSkipsWithoutRepo — no GH_REPO, no probe. Missing context is
// never the token's fault and must not fail a run.
func TestValuesRepoProbeSkipsWithoutRepo(t *testing.T) {
	orig := GitRefsProbe
	t.Cleanup(func() { GitRefsProbe = orig })
	called := false
	GitRefsProbe = func(_, _, _ string) (int, error) { called = true; return 200, nil }

	t.Setenv("GH_REPO", "")
	if cr := probeCapability(testCapCtx(), capCheckFor(t, "APL_VALUES_REPO_TOKEN"), "tok"); cr.Status != CapSkipped {
		t.Errorf("status = %v, want CapSkipped", cr.Status)
	}
	if called {
		t.Error("probed without the context to build a path")
	}
}

// TestValuesRepoHintNamesSSO guards the remediation against collapsing to
// "needs Contents: write". That alone is incomplete: an SSO-enforced org refuses
// an unauthorized PAT at the git endpoint no matter how it is scoped, and an
// operator who only re-checks the permission toggle finds nothing wrong.
func TestValuesRepoHintNamesSSO(t *testing.T) {
	h := CapabilityHint("APL_VALUES_REPO_TOKEN", capCheckFor(t, "APL_VALUES_REPO_TOKEN").op)
	if !strings.Contains(h, "Contents: write") {
		t.Errorf("hint must name the Contents permission; got %q", h)
	}
	if !strings.Contains(h, "SAML SSO") {
		t.Errorf("hint must name SSO authorization as the other cause; got %q", h)
	}
}
