package tokeninv

// validate_denied_count_test.go — the preflight's denial count is of
// CREDENTIALS, and its summary line says so.
//
// OPENBAO_SECRETS_WRITE_TOKEN carries two required grants, so the loop that
// reports them runs twice for one token. Incrementing the counter inside it made
// a single under-scoped PAT annotate the run with "2 REQUIRED pipeline
// credential(s) authenticate but lack a required scope" and summarise
// "2 scope-denied" — sending whoever reads the log hunting for a second broken
// credential that does not exist, in a preflight whose whole value is telling
// them precisely what to go and fix.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tokenprobe"
)

func TestValidate_DeniedCountIsPerCredentialNotPerGrant(t *testing.T) {
	origGH, origCap := tokenprobe.GHPATProbe, tokenprobe.GHCapabilityProbe
	t.Cleanup(func() { tokenprobe.GHPATProbe, tokenprobe.GHCapabilityProbe = origGH, origCap })
	tokenprobe.GHPATProbe = func(_, _ string) (int, string, error) { return 200, "", nil }
	// BOTH of this PAT's grants refused — the live shape of the outage.
	tokenprobe.GHCapabilityProbe = func(_, _, _ string) (int, error) { return 403, nil }

	for _, n := range validatableTokens {
		t.Setenv(n, "")
	}
	t.Setenv("TF_STATE_ACCESS_KEY", "")
	t.Setenv("TF_STATE_SECRET_KEY", "")
	t.Setenv("GHCR_USERNAME", "")
	t.Setenv("GH_REPO", "acme/platform")
	t.Setenv("REGION", "prod")
	t.Setenv("OPENBAO_SECRETS_WRITE_TOKEN", "valid-but-under-scoped")

	out := captureStdout(t, func() { _ = RunValidate(false) })

	// The fixture has to actually exercise both grants, or this proves nothing.
	if n := strings.Count(out, "└ scope"); n != 2 {
		t.Fatalf("scope lines = %d, want 2 (both grants reported for the one PAT)", n)
	}
	if !strings.Contains(out, "1 scope-denied") {
		t.Errorf("summary must count ONE denied credential, not its refusals; got:\n%s", out)
	}

	// And the blocking error names one credential too.
	err := RunValidate(true)
	if err == nil {
		t.Fatal("a refused required grant must block")
	}
	if !strings.Contains(err.Error(), "1 REQUIRED") {
		t.Errorf("the refusal must name ONE credential; got %q", err)
	}
}
