package main

import (
	"strings"
	"testing"
	"time"
)

// stubRunLookup points the poll loop at a fake and makes it instant.
func stubRunLookup(t *testing.T, attempts int, runs func(repo, workflow string) (dispatchedRun, bool)) {
	t.Helper()
	origSleep, origLatest, origAttempts := sleepFn, latestDispatchRun, runPollAttempts
	t.Cleanup(func() { sleepFn, latestDispatchRun, runPollAttempts = origSleep, origLatest, origAttempts })
	sleepFn = func(time.Duration) {}
	runPollAttempts = attempts
	latestDispatchRun = runs
}

func TestDispatchWatchRejectsTheRunThatPredatesTheDispatch(t *testing.T) {
	// THE bug this file exists to prevent. On an established instance the newest
	// workflow_dispatch run is a COMPLETED run from days ago until GitHub registers
	// the new one. Printing it hands the operator a `gh run watch` that returns
	// instantly green on somebody else's success.
	stubRunLookup(t, 2, func(string, string) (dispatchedRun, bool) {
		return dispatchedRun{ID: 100, URL: "https://x/100", Status: "completed"}, true
	})

	w := dispatchWatch{repo: "acme/inst", workflow: "terraform.yml", sinceID: 100, armed: true}
	out := captureStderr(t, w.report)

	if strings.Contains(out, "https://x/100") {
		t.Fatalf("printed the pre-dispatch run as if it were the new one:\n%s", out)
	}
	if !strings.Contains(out, "gh run list") {
		t.Errorf("must still give a route to find the run:\n%s", out)
	}
}

func TestDispatchWatchAcceptsAStrictlyNewerRun(t *testing.T) {
	stubRunLookup(t, 5, func(string, string) (dispatchedRun, bool) {
		return dispatchedRun{ID: 101, URL: "https://x/101", Status: "queued"}, true
	})

	w := dispatchWatch{repo: "acme/inst", workflow: "terraform.yml", sinceID: 100, armed: true}
	out := captureStderr(t, w.report)

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
	w := dispatchWatch{sinceID: 0, armed: true}
	if w.isOurs(dispatchedRun{ID: 7, Status: "completed"}) {
		t.Error("a completed run cannot be the one just dispatched")
	}
	if !w.isOurs(dispatchedRun{ID: 7, Status: "in_progress"}) {
		t.Error("a live run is the best available answer without a baseline")
	}
}

func TestDispatchWatchIsDisarmedWhenNothingWasDispatched(t *testing.T) {
	// --dry-run and a missing --yes dispatch nothing, so there is no run to point
	// at and no reason to spend API calls or wall-clock looking for one.
	for _, g := range []globalOpts{{DryRun: true, Yes: true}, {Yes: false}} {
		if beginDispatchWatch(g, "terraform.yml").armed {
			t.Errorf("watch must be disarmed for %+v", g)
		}
	}
	// A disarmed watch prints nothing at all.
	if out := captureStderr(t, dispatchWatch{}.report); out != "" {
		t.Errorf("disarmed report printed %q", out)
	}
}
