package dispatchwatch

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// stubRunLookup points the poll loop at a fake and makes it instant.
func stubRunLookup(t *testing.T, attempts int, runs func(repo, workflow string) (Run, bool)) {
	t.Helper()
	origSleep, origLatest, origAttempts := sleepFn, LatestDispatchRun, runPollAttempts
	t.Cleanup(func() { sleepFn, LatestDispatchRun, runPollAttempts = origSleep, origLatest, origAttempts })
	sleepFn = func(time.Duration) {}
	runPollAttempts = attempts
	LatestDispatchRun = runs
}

func TestDispatchWatchRejectsTheRunThatPredatesTheDispatch(t *testing.T) {
	// THE bug this file exists to prevent. On an established instance the newest
	// workflow_dispatch run is a COMPLETED run from days ago until GitHub registers
	// the new one. Printing it hands the operator a `gh run watch` that returns
	// instantly green on somebody else's success.
	stubRunLookup(t, 2, func(string, string) (Run, bool) {
		return Run{ID: 100, URL: "https://x/100", Status: "completed"}, true
	})

	w := Watch{repo: "acme/inst", workflow: "terraform.yml", sinceID: 100, armed: true}
	out := captureStderr(t, w.Report)

	if strings.Contains(out, "https://x/100") {
		t.Fatalf("printed the pre-dispatch run as if it were the new one:\n%s", out)
	}
	if !strings.Contains(out, "gh run list") {
		t.Errorf("must still give a route to find the run:\n%s", out)
	}
}

func TestDispatchWatchAcceptsAStrictlyNewerRun(t *testing.T) {
	stubRunLookup(t, 5, func(string, string) (Run, bool) {
		return Run{ID: 101, URL: "https://x/101", Status: "queued"}, true
	})

	w := Watch{repo: "acme/inst", workflow: "terraform.yml", sinceID: 100, armed: true}
	out := captureStderr(t, w.Report)

	for _, want := range []string{"https://x/101", "gh run watch 101", "gh run view 101", "first-build-failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDispatchWatchWithoutABaselineRejectsACompletedRun(t *testing.T) {
	// The pre-dispatch lookup can fail (rate limit, transient 5xx). With no
	// baseline the id test is unavailable, so a COMPLETED run must still be
	// refused — that single check is what stops the week-old-green-run failure.
	w := Watch{sinceID: 0, armed: true}
	if w.isOurs(Run{ID: 7, Status: "completed"}) {
		t.Error("a completed run cannot be the one just dispatched")
	}
	if !w.isOurs(Run{ID: 7, Status: "in_progress"}) {
		t.Error("a live run is the best available answer without a baseline")
	}
}

func TestDispatchWatchIsDisarmedWhenNothingWasDispatched(t *testing.T) {
	// --dry-run and a missing --yes dispatch nothing, so there is no run to point
	// at and no reason to spend API calls or wall-clock looking for one.
	for _, tc := range []struct{ dryRun, yes bool }{{dryRun: true, yes: true}, {yes: false}} {
		if Begin(tc.dryRun, tc.yes, "terraform.yml").armed {
			t.Errorf("watch must be disarmed for dryRun=%v yes=%v", tc.dryRun, tc.yes)
		}
	}
	// A disarmed watch prints nothing at all.
	if out := captureStderr(t, Watch{}.Report); out != "" {
		t.Errorf("disarmed report printed %q", out)
	}
}

// captureStderr travelled with these tests out of package cli, where several
// unrelated suites still use the original. A copy rather than a shared helper:
// exporting a test-only utility from another package to save nine lines is how
// production packages grow test dependencies.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = orig
	var b strings.Builder
	if _, err := io.Copy(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
