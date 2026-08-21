package instanceresolve

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

// TestTheSkipNoticeReachesTheRunSummaryUnderActions — the notice's whole job is to
// make "this check did not run" visible, and in CI it was not.
//
// On a laptop it is a yellow paragraph nobody misses. In a workflow it was plain
// step-log text inside a GREEN step, which is exactly the state it exists to
// expose: a run that validated nothing looks identical to one that validated
// everything. `llz ci assert-k8s-version` made this same call for the same reason.
//
// The repo's own e2e lane is what forced it — its scaffold runs in the template
// repo where a Linode token is optional, so the alternative was three lines of
// `if [[ -z "$TOKEN" ]]` in the workflow: untestable inline bash the budget gate
// refuses, covering one caller where this covers every one.
func TestTheSkipNoticeReachesTheRunSummaryUnderActions(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	resetAccountCheckSkip()
	out := captureStderr(t, func() { reportSkippedAccountCheck("--region", nil) })

	if !strings.HasPrefix(strings.TrimSpace(out), "::warning::") {
		t.Errorf("under Actions the notice must be an ANNOTATION, or it is a green step with a\n"+
			"paragraph nobody reads. got:\n%s", out)
	}
	// A WORKFLOW COMMAND ENDS AT THE FIRST RAW NEWLINE, silently — everything after
	// it falls out of the annotation into step-log text, which is the half that says
	// what to do. cigate.Annotation escapes them; this is what proves it ran.
	if strings.Contains(strings.TrimSuffix(out, "\n"), "\n") {
		t.Errorf("the annotation carries a raw newline, so Actions truncates it at that point and the\n"+
			"remedy never appears in the summary. got:\n%q", out)
	}
	// And the reason has to survive the escaping, not just the headline.
	for _, want := range []string{"LINODE_TOKEN", "us-sea-1", "terraform apply"} {
		if !strings.Contains(out, want) {
			t.Errorf("the annotation must still carry %q after escaping; got:\n%s", want, out)
		}
	}
	// IT MUST NOT SPEAK FOR A COMMAND THAT DID NOT CALL IT. This notice is shared —
	// `llz reap` reaches it through AccountRegions to sweep orphaned resources and
	// seeds no spec — so a sentence about cluster.k8sVersion falling back to a
	// compiled default was untrue wherever it was not `llz env add`. That command
	// reports its own version consequence in its banner, beside the value.
	for _, forbidden := range []string{"k8sVersion", "compiled default"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("the shared notice claims %q, but it also fires from `llz reap`, which writes no "+
				"spec — a consequence belongs to the command that has it, not to this notice; got:\n%s",
				forbidden, out)
		}
	}

	// Outside Actions it stays a readable paragraph — `%0A` in place of the remedy
	// is its own way of hiding it, and this verb runs on laptops.
	t.Setenv("GITHUB_ACTIONS", "")
	resetAccountCheckSkip()
	plain := captureStderr(t, func() { reportSkippedAccountCheck("--region", nil) })
	if strings.Contains(plain, "%0A") || strings.Contains(plain, "::warning::") {
		t.Errorf("off Actions the notice must be plain multi-line text, not an escaped workflow command:\n%s", plain)
	}
	if !strings.Contains(plain, "\n") {
		t.Errorf("off Actions the notice should keep its line breaks:\n%s", plain)
	}
}

// TestFirstLineKeepsTheAnnotationOnOneLine — firstLine is what stops a multi-line
// API error from carrying a raw newline into a workflow command, where everything
// after it is silently dropped. The newline case was the untested half, which is
// the only half that does anything.
func TestFirstLineKeepsTheAnnotationOnOneLine(t *testing.T) {
	if got := firstLine("Get \"https://api.linode.com/…\": 401\nInvalid Token\nCheck the PAT"); got != "Get \"https://api.linode.com/…\": 401" {
		t.Errorf("firstLine(multi-line) = %q, want only the first line", got)
	}
	if got := firstLine("401 Invalid Token"); got != "401 Invalid Token" {
		t.Errorf("firstLine(single-line) = %q, want it unchanged", got)
	}
	// End to end: a multi-line cause must not smuggle a newline into the annotation.
	t.Setenv("GITHUB_ACTIONS", "true")
	resetAccountCheckSkip()
	out := captureStderr(t, func() {
		reportSkippedAccountCheck("--k8s-version", errors.New("dial tcp: i/o timeout\nsecond line\nthird"))
	})
	if strings.Contains(strings.TrimSuffix(out, "\n"), "\n") {
		t.Errorf("a multi-line cause truncated the annotation: %q", out)
	}
	if strings.Contains(out, "second line") {
		t.Errorf("only the first line of the cause belongs in the annotation: %q", out)
	}
}
