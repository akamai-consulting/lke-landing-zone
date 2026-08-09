package tokeninv

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tokenprobe"
)

// clearValidatableTokens blanks every credential this preflight reads, so a test
// controls exactly which ones are "present".
func clearValidatableTokens(t *testing.T) {
	t.Helper()
	for _, n := range validatableTokens {
		t.Setenv(n, "")
	}
	t.Setenv("GHCR_USERNAME", "")
	t.Setenv("GH_REPO", "")
	t.Setenv("REGION", "")
	t.Setenv("TF_STATE_ACCESS_KEY", "")
	t.Setenv("TF_STATE_SECRET_KEY", "")
	t.Setenv("TF_STATE_ENDPOINT", "")
	t.Setenv("TF_STATE_BUCKET", "")
}

// TestValidateTokensSummaryCountsAreHonest pins the tally line. The verb exits 0
// whenever nothing BLOCKING is wrong, so on a color.Green run this one line is the
// entire report — "probed 0 credential(s)" on a run that probed three is how a
// silently-empty environment (a mis-scoped GH Environment handing the job no
// secrets at all) passes as a clean preflight.
func TestValidateTokensSummaryCountsAreHonest(t *testing.T) {
	origLinode, origGHCR, origGH := tokenprobe.LinodeProbe, tokenprobe.GHCRTokenProbe, tokenprobe.GHPATProbe
	t.Cleanup(func() {
		tokenprobe.LinodeProbe, tokenprobe.GHCRTokenProbe, tokenprobe.GHPATProbe = origLinode, origGHCR, origGH
	})
	tokenprobe.LinodeProbe = func(string) (int, error) { return 200, nil }
	tokenprobe.GHCRTokenProbe = func(_, _ string) (int, error) { return 403, nil } // GHCR is optional
	tokenprobe.GHPATProbe = func(_, _ string) (int, string, error) { return 200, "", nil }

	clearValidatableTokens(t)
	t.Setenv("LINODE_API_TOKEN", "live")   // required, valid
	t.Setenv("GHCR_READ_TOKEN", "stale")   // optional, invalid → warn only
	t.Setenv("LINODE_DNS_TOKEN", "live-2") // optional, valid

	var err error
	out := captureStdout(t, func() { err = RunValidate(true) })
	if err != nil {
		t.Fatalf("only an OPTIONAL credential is invalid, so this must exit 0: %v", err)
	}
	// Matched as one whole line: "-1 optional-invalid" contains "1 optional-invalid".
	const want = "probed 3 credential(s): 0 blocking-invalid, 1 optional-invalid, 0 scope-denied."
	if !strings.Contains(out, want) {
		t.Errorf("summary line wrong — want exactly:\n  %s\ngot:\n%s", want, out)
	}
}

// TestValidateTokensProbesTheStateKeyPairWhenBothAreSet: the OBJ state-bucket
// key pair is REQUIRED and is the one credential validated as a pair — a
// half-set pair cannot be signed with, so it is skipped, but a fully-set one
// must be probed and counted. Losing this branch means an expired state key is
// discovered by `tofu init` instead of by the preflight.
func TestValidateTokensProbesTheStateKeyPairWhenBothAreSet(t *testing.T) {
	origLinode, origGHCR, origGH := tokenprobe.LinodeProbe, tokenprobe.GHCRTokenProbe, tokenprobe.GHPATProbe
	t.Cleanup(func() {
		tokenprobe.LinodeProbe, tokenprobe.GHCRTokenProbe, tokenprobe.GHPATProbe = origLinode, origGHCR, origGH
	})
	tokenprobe.LinodeProbe = func(string) (int, error) { return 200, nil }
	tokenprobe.GHCRTokenProbe = func(_, _ string) (int, error) { return 200, nil }
	tokenprobe.GHPATProbe = func(_, _ string) (int, string, error) { return 200, "", nil }

	// Endpoint/bucket deliberately left unset: ProbeS3Pair reports "can't sign a
	// probe" without touching the network, which is enough to observe the branch.
	clearValidatableTokens(t)
	t.Setenv("TF_STATE_ACCESS_KEY", "AK")
	t.Setenv("TF_STATE_SECRET_KEY", "SK")

	var err error
	out := captureStdout(t, func() { err = RunValidate(true) })
	if err != nil {
		t.Fatalf("an unprobeable (but present) key pair must not block: %v", err)
	}
	if !strings.Contains(out, "TF_STATE_ACCESS_KEY/SECRET") {
		t.Errorf("a fully-set OBJ key pair must be reported:\n%s", out)
	}
	if !strings.Contains(out, "probed 1 credential(s):") {
		t.Errorf("the key pair counts as one probed credential:\n%s", out)
	}

	// Only one half set → nothing to sign with → not probed at all.
	for _, half := range []string{"TF_STATE_ACCESS_KEY", "TF_STATE_SECRET_KEY"} {
		clearValidatableTokens(t)
		t.Setenv(half, "only-one")
		out := captureStdout(t, func() { err = RunValidate(true) })
		if err != nil {
			t.Fatalf("%s alone must not block: %v", half, err)
		}
		if strings.Contains(out, "TF_STATE_ACCESS_KEY/SECRET") {
			t.Errorf("with only %s set there is nothing to sign a probe with:\n%s", half, out)
		}
		if !strings.Contains(out, "probed 0 credential(s)") {
			t.Errorf("with only %s set nothing should be probed:\n%s", half, out)
		}
	}
}
