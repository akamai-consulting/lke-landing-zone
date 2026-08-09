package assertplatform

// Gap-closing tests for ci_assert_argo_app.go surfaced by mutation testing. The
// existing suite proves each POLICY (git-auth is terminal after the grace, a
// transient fetch error is nudged, a real manifest error is not) but never pins
// the boundaries those policies are made of: WHEN the grace expires, HOW OFTEN a
// nudge may fire, and how the parent's operationState is split into the phase and
// message the failure annotation prints.

import (
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
)

// The grace is a deadline, not a suggestion: a refusal that has persisted for
// gitAuthGrace is terminal RIGHT THEN. One cadence later is a different verdict —
// with a budget that ends on the boundary the gate reports "not created within"
// (go look at the sync waves) instead of "cannot authenticate" (go look at the
// token), which is the misdiagnosis the grace was added to avoid.
func TestAssertArgoAppGitAuthIsTerminalAtTheGraceBoundary(t *testing.T) {
	d, _ := assertArgoAppDeps(t, func(_ int, args []string) (string, bool) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "get application.argoproj.io platform-openbao"):
			return "", false // never appears
		case strings.Contains(joined, "ComparisonError"):
			return "failed to list refs: authentication required: Unauthorized", true
		default:
			return "\t", true // no sync operation started
		}
	})
	// Budget == grace: the two expire on the same tick, and the git-auth verdict
	// must be the one that lands.
	err := assertArgoApp(d, "argocd", "platform-openbao", "platform-bootstrap", gitAuthGrace)
	if err == nil {
		t.Fatal("a git-auth refusal that reaches the grace must abort")
	}
	if !strings.Contains(err.Error(), "cannot authenticate to the source repo") {
		t.Fatalf("err = %v, want the terminal git-auth verdict — the gate waited a cadence too long and fell through to the deadline arm", err)
	}
}

// The hard-refresh nudge is throttled to 20s so a wedged parent is re-fetched
// promptly without hammering the repo-server. Both halves are asserted at once by
// counting nudges over a fixed window at the loop's 10s cadence: a 60s window
// admits exactly four (t=0, 20, 40, 60).
func TestAssertArgoAppThrottlesTheHardRefreshToTwentySeconds(t *testing.T) {
	refreshes := 0
	d, _ := assertArgoAppDeps(t, func(_ int, args []string) (string, bool) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "annotate") && strings.Contains(joined, "refresh=hard"):
			refreshes++
			return "", true
		case strings.Contains(joined, "get application.argoproj.io platform-openbao"):
			return "", false // never appears
		case strings.Contains(joined, "ComparisonError"):
			return "rpc error: code = Unknown desc = failed to list refs: repository not found", true
		default:
			return "\t", true
		}
	})
	err := assertArgoApp(d, "argocd", "platform-openbao", "platform-bootstrap", 60*time.Second)
	if err == nil || !strings.Contains(err.Error(), "not created within") {
		t.Fatalf("err = %v, want the deadline verdict", err)
	}
	if refreshes != 4 {
		t.Errorf("forced %d hard refreshes across a 60s window, want 4 (t=0,20,40,60) — the 20s throttle is wrong: fewer means the re-fetch is late, more means the repo-server is being hammered every cycle", refreshes)
	}
}

// argoOperationState splits `<phase>\t<message>` — and the MESSAGE is the root
// cause the failure annotation prints. It must survive a parent that has no
// message at all (no tab in the jsonpath output), which is the normal shape when
// no sync operation ever started.
func TestArgoOperationStateSplitsPhaseFromMessage(t *testing.T) {
	for _, c := range []struct{ name, out, phase, msg string }{
		{"phase and message", "Failed\tone or more synchronization tasks completed unsuccessfully", "Failed", "one or more synchronization tasks completed unsuccessfully"},
		{"phase only, no tab", "Running", "Running", ""},
		{"empty operationState", "\t", "", ""},
		{"message carries tabs of its own", "Error\tstep 1\tstep 2", "Error", "step 1\tstep 2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, _ := assertArgoAppDeps(t, func(_ int, _ []string) (string, bool) { return c.out, true })
			phase, msg := argoOperationState(d, "argocd", "platform-bootstrap")
			if phase != c.phase || msg != c.msg {
				t.Errorf("argoOperationState(%q) = (%q, %q), want (%q, %q)", c.out, phase, msg, c.phase, c.msg)
			}
		})
	}

	// An unreadable parent yields empty values, not a partial read.
	d, _ := assertArgoAppDeps(t, func(_ int, _ []string) (string, bool) { return "Failed\tboom", false })
	if phase, msg := argoOperationState(d, "argocd", "platform-bootstrap"); phase != "" || msg != "" {
		t.Errorf("unreadable parent = (%q, %q), want empty", phase, msg)
	}
}

// firstLine is what the progress log shows for a condition message. Both of its
// cuts have an exact boundary: a leading newline means the first line is EMPTY,
// and the length cap must not append an ellipsis to a message that already fits.
func TestFirstLineCuts(t *testing.T) {
	if got := cigate.FirstLine("failed to list refs\nrepository not found"); got != "failed to list refs" {
		t.Errorf("cigate.FirstLine(multi-line) = %q", got)
	}
	if got := cigate.FirstLine("\nthe real text is on line 2"); got != "" {
		t.Errorf("cigate.FirstLine(leading newline) = %q, want \"\" — the first line is empty", got)
	}
	if got := cigate.FirstLine(""); got != "" {
		t.Errorf("cigate.FirstLine(\"\") = %q, want empty", got)
	}

	exactly140 := strings.Repeat("x", 140)
	if got := cigate.FirstLine(exactly140); got != exactly140 {
		t.Errorf("cigate.FirstLine(140 chars) truncated a message that fits: len=%d, ellipsis=%v", len(got), strings.Contains(got, "…"))
	}
	over := strings.Repeat("x", 141)
	got := cigate.FirstLine(over)
	if !strings.HasSuffix(got, "…") || strings.Count(got, "x") != 140 {
		t.Errorf("cigate.FirstLine(141 chars) = %q, want the first 140 plus an ellipsis", got)
	}
}
