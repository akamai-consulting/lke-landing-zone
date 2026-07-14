package main

import "testing"

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
	if code := runCIValidateTokens(true); code != 1 {
		t.Errorf("invalid required token: exit %d, want 1", code)
	}

	// A dead OPTIONAL token (GHCR) only → warning → exit 0.
	linodeProbe = func(string) (int, error) { return 200, nil }
	ghcrTokenProbe = func(_, _ string) (int, error) { return 403, nil }
	clearAll()
	t.Setenv("GHCR_READ_TOKEN", "stale")
	if code := runCIValidateTokens(true); code != 0 {
		t.Errorf("invalid optional token: exit %d, want 0 (warn only)", code)
	}

	// Blocking-invalid but --fail-on-invalid=false → report only → exit 0.
	linodeProbe = func(string) (int, error) { return 401, nil }
	clearAll()
	t.Setenv("LINODE_API_TOKEN", "dead")
	if code := runCIValidateTokens(false); code != 0 {
		t.Errorf("fail-on-invalid=false: exit %d, want 0", code)
	}
}
