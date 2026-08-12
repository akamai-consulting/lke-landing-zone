package dispatchwatch

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghapi"
)

// stubRunLookup points the poll loop at a fake and makes it instant.
func stubRunLookup(t *testing.T, attempts int, runs func(repo, workflow string) ([]Run, bool)) {
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
	stubRunLookup(t, 2, func(string, string) ([]Run, bool) {
		return []Run{{ID: 100, URL: "https://x/100", Status: "completed"}}, true
	})

	w := Watch{repo: "acme/inst", workflow: "terraform.yml", sinceID: 100, baseline: true, armed: true}
	out := captureStderr(t, w.Report)

	if strings.Contains(out, "https://x/100") {
		t.Fatalf("printed the pre-dispatch run as if it were the new one:\n%s", out)
	}
	if !strings.Contains(out, "gh run list") {
		t.Errorf("must still give a route to find the run:\n%s", out)
	}
}

func TestDispatchWatchAcceptsAStrictlyNewerRun(t *testing.T) {
	stubRunLookup(t, 5, func(string, string) ([]Run, bool) {
		return []Run{{ID: 101, URL: "https://x/101", Status: "queued"}}, true
	})

	w := Watch{repo: "acme/inst", workflow: "terraform.yml", sinceID: 100, baseline: true, armed: true}
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
	w := Watch{sinceID: 0, baseline: false, armed: true}
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

// Begin2ForTest builds a Watch through the same baseline path Begin uses, without
// Begin's gh/answers probes — so the tests cover how `baseline` is SET, not a
// hand-made struct that could disagree with it.
func Begin2ForTest(repo, workflow string) Watch {
	w := Watch{repo: repo, workflow: workflow, armed: true}
	if runs, ok := LatestDispatchRun(repo, workflow); ok {
		w.baseline = true
		for _, r := range runs {
			if r.ID > w.sinceID {
				w.sinceID = r.ID
			}
		}
	}
	return w
}

// TestLatestDispatchRunTreatsAnEmptyPageAsAnAnswer covers the REAL function
// rather than the seam every other test replaces.
//
// That gap is why the bug survived a fix: the empty-list case was "fixed" by
// editing the comment and the tail return while the early return still bailed
// with ok=false, and no test could see it because they all stub
// LatestDispatchRun. Here the ghapi transport is stubbed instead, so the
// function's own branching is what runs.
func TestLatestDispatchRunTreatsAnEmptyPageAsAnAnswer(t *testing.T) {
	orig := ghapi.GHAPIJSON
	t.Cleanup(func() { ghapi.GHAPIJSON = orig })

	ghapi.GHAPIJSON = func(_ string, out any) error {
		return json.Unmarshal([]byte(`{"workflow_runs":[]}`), out)
	}
	runs, ok := LatestDispatchRun("acme/inst", "terraform.yml")
	if !ok {
		t.Error("an empty run list is an ANSWER — this workflow has never been dispatched. Reporting it " +
			"as a failed query makes every first-ever dispatch fail its watch for an apply that is running.")
	}
	if len(runs) != 0 {
		t.Errorf("expected no runs, got %d", len(runs))
	}

	ghapi.GHAPIJSON = func(string, any) error { return errBoom{} }
	if _, ok := LatestDispatchRun("acme/inst", "terraform.yml"); ok {
		t.Error("an unreachable API must still report failure — that is the case Wait() refuses on")
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }
