package main

import (
	"errors"
	"strings"
	"testing"
)

func withAccountSkipReset(t *testing.T) {
	t.Helper()
	resetAccountCheckSkip()
	t.Cleanup(resetAccountCheckSkip)
}

func TestSkippedAccountCheckSeparatesNoTokenFromRejectedToken(t *testing.T) {
	// The two cases have DIFFERENT fixes, which is the whole reason this exists:
	// no token means a quickstart step was skipped; a rejected token means the PAT
	// is dead or under-scoped and will fail the build too. Collapsing them was the
	// original bug (both were silent).
	withAccountSkipReset(t)
	noTok := captureStderr(t, func() { reportSkippedAccountCheck("--region", nil) })

	withAccountSkipReset(t)
	badTok := captureStderr(t, func() {
		reportSkippedAccountCheck("--obj-cluster", errors.New("401 Invalid Token"))
	})

	if !strings.Contains(noTok, "no LINODE_TOKEN is set") {
		t.Errorf("missing-token case must name the missing variable:\n%s", noTok)
	}
	if !strings.Contains(noTok, "export LINODE_TOKEN") {
		t.Errorf("missing-token case must give the fix:\n%s", noTok)
	}
	if strings.Contains(badTok, "no LINODE_TOKEN is set") {
		t.Errorf("a REJECTED token must not be reported as an absent one:\n%s", badTok)
	}
	for _, want := range []string{"401 Invalid Token", "expired", "llz doctor"} {
		if !strings.Contains(badTok, want) {
			t.Errorf("rejected-token case missing %q:\n%s", want, badTok)
		}
	}
}

func TestSkippedAccountCheckSpeaksOnlyOnce(t *testing.T) {
	// checkRegion and resolveOBJCluster both run in one `llz env add`; the same
	// paragraph twice reads as two different problems.
	withAccountSkipReset(t)
	first := captureStderr(t, func() { reportSkippedAccountCheck("--region", nil) })
	second := captureStderr(t, func() { reportSkippedAccountCheck("--obj-cluster", nil) })

	if first == "" {
		t.Fatal("the first call must report")
	}
	if second != "" {
		t.Errorf("the second call must stay quiet, got:\n%s", second)
	}
}

func TestSkippedAccountCheckIsResettable(t *testing.T) {
	// Guards the test hazard the global creates: without a reset the first case to
	// trip it silences every later one, and assertions start depending on order.
	withAccountSkipReset(t)
	_ = captureStderr(t, func() { reportSkippedAccountCheck("--region", nil) })
	resetAccountCheckSkip()
	if out := captureStderr(t, func() { reportSkippedAccountCheck("--region", nil) }); out == "" {
		t.Error("resetAccountCheckSkip must re-arm the notice")
	}
}

func TestFirstNonNilErrPrefersTheRealCause(t *testing.T) {
	real := errors.New("503")
	if got := firstNonNilErr(real, errEmptyAccountListing); got != real {
		t.Errorf("got %v, want the real cause", got)
	}
	if got := firstNonNilErr(nil, errEmptyAccountListing); got != errEmptyAccountListing {
		t.Errorf("got %v, want the fallback", got)
	}
}
