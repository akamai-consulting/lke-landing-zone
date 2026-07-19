package main

import (
	"strings"
	"testing"
)

// TestClassifyCapabilityStatus pins the verdict table, and specifically the two
// codes that must NOT be conflated: 403 (authenticated but under-scoped → block)
// versus 404 (target absent OR invisible → ambiguous, warn only). Blocking on
// 404 would trade a late true positive for an early false one.
func TestClassifyCapabilityStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		want capabilityStatus
	}{
		{"authorized", 200, capOK},
		{"no content", 204, capOK},
		{"under-scoped", 403, capDenied},
		{"rejected", 401, capDenied},
		{"ambiguous", 404, capUnknown},
		{"unreachable", 0, capUnknown},
		{"server error", 500, capUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := classifyCapabilityStatus(tc.code, "do the thing")
			if got != tc.want {
				t.Errorf("code %d: status %v, want %v", tc.code, got, tc.want)
			}
			if got != capOK && detail == "" {
				t.Errorf("code %d: want a non-empty detail explaining the verdict", tc.code)
			}
		})
	}

	// A denial must say the token is under-scoped, NOT that it expired — the
	// remediation is re-scoping, and "rotate it" sends the operator the wrong way.
	_, detail := classifyCapabilityStatus(403, "write environment secrets")
	if !strings.Contains(detail, "under-scoped") {
		t.Errorf("403 detail = %q, want it to name the scope as the cause", detail)
	}
}

// TestProbeCapability_SkipsWithoutContext verifies that a missing GH_REPO/REGION
// skips rather than fails. The probe can't be built without them, and that is
// never the token's fault.
func TestProbeCapability_SkipsWithoutContext(t *testing.T) {
	orig := ghCapabilityProbe
	t.Cleanup(func() { ghCapabilityProbe = orig })
	called := false
	ghCapabilityProbe = func(_, _, _ string) (int, error) { called = true; return 200, nil }

	t.Setenv("GH_REPO", "")
	t.Setenv("REGION", "")
	cr := probeCapability(capabilityChecks[0], "tok")
	if cr.status != capSkipped {
		t.Errorf("status = %v, want capSkipped", cr.status)
	}
	if called {
		t.Error("probed the API without the context to build a path")
	}
}

// TestProbeCapability_ProbesTheRealEndpoint asserts the seal-key check hits the
// exact path `gh secret set --env infra-<region>` fetches. If this drifts, the
// check stops being the read-only twin of the real call and starts guessing.
func TestProbeCapability_ProbesTheRealEndpoint(t *testing.T) {
	orig := ghCapabilityProbe
	t.Cleanup(func() { ghCapabilityProbe = orig })
	var gotPath string
	ghCapabilityProbe = func(_, _, path string) (int, error) { gotPath = path; return 403, nil }

	t.Setenv("GH_REPO", "acme/platform")
	t.Setenv("REGION", "prod")
	cr := probeCapability(capabilityChecks[0], "tok")

	const want = "/repos/acme/platform/environments/infra-prod/secrets/public-key"
	if gotPath != want {
		t.Errorf("probed %q, want %q", gotPath, want)
	}
	if cr.status != capDenied {
		t.Errorf("403 → status %v, want capDenied", cr.status)
	}
}

// TestCheckCapability_OnlyRegisteredTokens confirms credentials with no scope
// requirement report nothing (rather than a bogus verdict).
func TestCheckCapability_OnlyRegisteredTokens(t *testing.T) {
	orig := ghCapabilityProbe
	t.Cleanup(func() { ghCapabilityProbe = orig })
	ghCapabilityProbe = func(_, _, _ string) (int, error) { return 200, nil }
	t.Setenv("GH_REPO", "acme/platform")
	t.Setenv("REGION", "prod")

	if _, ok := checkCapability("LINODE_API_TOKEN", "tok"); ok {
		t.Error("LINODE_API_TOKEN has no registered scope check, want ok=false")
	}
	if _, ok := checkCapability("OPENBAO_SECRETS_WRITE_TOKEN", "tok"); !ok {
		t.Error("OPENBAO_SECRETS_WRITE_TOKEN should have a scope check")
	}
	if h := capabilityHint("OPENBAO_SECRETS_WRITE_TOKEN"); h == "" {
		t.Error("a denial must carry remediation text")
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
	h := capabilityHint("OPENBAO_SECRETS_WRITE_TOKEN")
	if !strings.Contains(h, "Environments: write") {
		t.Errorf("hint must name the Environments permission; got %q", h)
	}
	if !strings.Contains(h, "Environment admin") {
		t.Errorf("hint must also name the Environment-admin requirement; got %q", h)
	}
}

// TestSecretsWritePATURLRequestsEnvironments pins the wizard's pre-filled PAT
// link to the permission that actually governs environment secrets. Every
// credential in catalog() is destined for an infra-<env> ENVIRONMENT secret, so
// a link that pre-fills `secrets=write` mints a token that cannot do the job.
func TestSecretsWritePATURLRequestsEnvironments(t *testing.T) {
	u := ghFineGrainedSecretsWriteURL("llz-openbao-secrets-write", "acme")
	if !strings.Contains(u, "environments=write") {
		t.Errorf("pre-fill must request environments=write; got %q", u)
	}
	if strings.Contains(u, "secrets=write") {
		t.Errorf("pre-fill must NOT request secrets=write (repo-level only, not environment secrets); got %q", u)
	}
	if !strings.Contains(u, "actions=write") {
		t.Errorf("pre-fill should keep actions=write (workflow dispatch); got %q", u)
	}
}
