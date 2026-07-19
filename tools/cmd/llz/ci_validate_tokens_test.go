package main

import (
	"strings"
	"testing"
)

// TestRunCIValidateTokens_OptionalVsRequired verifies the exit contract: an
// invalid REQUIRED credential fails the run, while an invalid OPTIONAL one
// (GHCR/DNS) only warns.
func TestRunCIValidateTokens_OptionalVsRequired(t *testing.T) {
	origLinode, origGHCR, origGH := linodeProbe, ghcrTokenProbe, ghPATProbe
	t.Cleanup(func() { linodeProbe, ghcrTokenProbe, ghPATProbe = origLinode, origGHCR, origGH })
	ghPATProbe = func(_, _ string) (int, string, error) { return 200, "", nil }

	clearAll := func() {
		for _, n := range validatableTokens {
			t.Setenv(n, "")
		}
		t.Setenv("GHCR_USERNAME", "")
	}

	// A dead REQUIRED token (Linode API) → blocking → exit 1.
	linodeProbe = func(string) (int, error) { return 401, nil }
	ghcrTokenProbe = func(_, _ string) (int, error) { return 200, nil }
	clearAll()
	t.Setenv("LINODE_API_TOKEN", "dead")
	if err := runCIValidateTokens(true); err == nil {
		t.Errorf("invalid required token: err %v, want non-nil", err)
	}

	// A dead OPTIONAL token (GHCR) only → warning → exit 0.
	linodeProbe = func(string) (int, error) { return 200, nil }
	ghcrTokenProbe = func(_, _ string) (int, error) { return 403, nil }
	clearAll()
	t.Setenv("GHCR_READ_TOKEN", "stale")
	if err := runCIValidateTokens(true); err != nil {
		t.Errorf("invalid optional token: err %v, want nil (warn only)", err)
	}

	// Blocking-invalid but --fail-on-invalid=false → report only → exit 0.
	linodeProbe = func(string) (int, error) { return 401, nil }
	clearAll()
	t.Setenv("LINODE_API_TOKEN", "dead")
	if err := runCIValidateTokens(false); err != nil {
		t.Errorf("fail-on-invalid=false: err %v, want nil", err)
	}
}

// TestRunCIValidateTokens_UnderScopedPAT is the regression test for the failure
// this preflight missed: OPENBAO_SECRETS_WRITE_TOKEN probed "✓ valid, expires in
// 77d" and then 403'd on `gh secret set --env infra-prod` six minutes later,
// after the cluster was already provisioned. A VALID but under-scoped credential
// must fail HERE.
func TestRunCIValidateTokens_UnderScopedPAT(t *testing.T) {
	origGH, origCap := ghPATProbe, ghCapabilityProbe
	t.Cleanup(func() { ghPATProbe, ghCapabilityProbe = origGH, origCap })
	// Authenticates cleanly with plenty of life left — exactly the real case.
	ghPATProbe = func(_, _ string) (int, string, error) { return 200, "", nil }

	for _, n := range validatableTokens {
		t.Setenv(n, "")
	}
	t.Setenv("GHCR_USERNAME", "")
	t.Setenv("GH_REPO", "acme/platform")
	t.Setenv("REGION", "prod")
	t.Setenv("OPENBAO_SECRETS_WRITE_TOKEN", "valid-but-under-scoped")

	// Denied the environment-secret write → blocking, even though it is valid.
	ghCapabilityProbe = func(_, _, _ string) (int, error) { return 403, nil }
	if err := runCIValidateTokens(true); err == nil {
		t.Error("valid but scope-denied token: err nil, want non-nil (this is the bug)")
	} else if !strings.Contains(err.Error(), "scope") {
		t.Errorf("err = %q, want it to name the SCOPE (re-scope, not rotate)", err)
	}

	// --fail-on-invalid=false still reports only.
	if err := runCIValidateTokens(false); err != nil {
		t.Errorf("fail-on-invalid=false: err %v, want nil", err)
	}

	// Authorized → clean run.
	ghCapabilityProbe = func(_, _, _ string) (int, error) { return 200, nil }
	if err := runCIValidateTokens(true); err != nil {
		t.Errorf("authorized token: err %v, want nil", err)
	}

	// Ambiguous 404 must NOT block — it cannot be told from "environment not
	// created yet", and a false denial is worse than the late true positive.
	ghCapabilityProbe = func(_, _, _ string) (int, error) { return 404, nil }
	if err := runCIValidateTokens(true); err != nil {
		t.Errorf("ambiguous 404: err %v, want nil (warn only)", err)
	}

	// No GH_REPO/REGION → cannot build the probe → must not block.
	ghCapabilityProbe = func(_, _, _ string) (int, error) { return 403, nil }
	t.Setenv("GH_REPO", "")
	t.Setenv("REGION", "")
	if err := runCIValidateTokens(true); err != nil {
		t.Errorf("no probe context: err %v, want nil (skipped)", err)
	}
}
