package dispatchwatch

import (
	"strings"
	"testing"
	"time"
)

// wait_test.go — the fail-closed arms of `llz build --watch`.
//
// report() is best-effort: the dispatch already happened, so nothing it does may
// fail the command. wait() has the OPPOSITE contract, because its caller is an
// unattended scheduled apply whose exit code is the only signal anyone sees. Each
// test below is one way the watcher can lose track of the run, and every one of
// them must be an error rather than a pass — a job that goes green because the
// watcher stopped looking is indistinguishable from an apply that worked.

// stubRunConclusion drives wait()'s status poll from a script of answers: one
// entry per poll, consumed in order, with the last entry repeating.
func stubRunConclusion(t *testing.T, attempts int, answers []conclusionAnswer) {
	t.Helper()
	origSleep, origConc, origAttempts := sleepFn, runConclusion, runWatchAttempts
	t.Cleanup(func() { sleepFn, runConclusion, runWatchAttempts = origSleep, origConc, origAttempts })
	sleepFn = func(time.Duration) {}
	runWatchAttempts = attempts
	i := 0
	runConclusion = func(string, uint64) (string, string, bool) {
		a := answers[min(i, len(answers)-1)]
		i++
		return a.status, a.conclusion, a.ok
	}
}

type conclusionAnswer struct {
	status     string
	conclusion string
	ok         bool
}

// captureStderrErr is captureStderr for a func that returns an error — wait()
// both prints progress and reports a verdict, and the tests here are about the
// verdict. Discards the output; the sibling report() tests cover the printing.
func captureStderrErr(t *testing.T, fn func() error) error {
	t.Helper()
	var err error
	captureStderr(t, func() { err = fn() })
	return err
}

// armedWatch is a watch that has already identified run 101 as its own.
func armedWatch(t *testing.T) Watch {
	t.Helper()
	stubRunLookup(t, 5, func(string, string) (Run, bool) {
		return Run{ID: 101, URL: "https://x/101", Status: "queued"}, true
	})
	return Watch{repo: "acme/inst", workflow: "terraform.yml", sinceID: 100, armed: true}
}

func TestWaitSucceedsOnlyOnASuccessfulRun(t *testing.T) {
	w := armedWatch(t)
	stubRunConclusion(t, 10, []conclusionAnswer{
		{status: "in_progress", ok: true},
		{status: "in_progress", ok: true},
		{status: "completed", conclusion: "success", ok: true},
	})
	if err := captureStderrErr(t, w.Wait); err != nil {
		t.Fatalf("a completed+success run must pass, got %v", err)
	}
}

func TestWaitFailsAndNamesTheConclusion(t *testing.T) {
	// "failed" and "startup_failure" send a reader to completely different places
	// — the latter produces no annotations at all, which is how a delivered
	// rotation workflow stayed dead for months. Collapsing both to "the run
	// failed" throws away the one word that picks the remedy.
	for _, conclusion := range []string{"failure", "cancelled", "timed_out", "startup_failure"} {
		w := armedWatch(t)
		stubRunConclusion(t, 10, []conclusionAnswer{{status: "completed", conclusion: conclusion, ok: true}})

		err := captureStderrErr(t, w.Wait)
		if err == nil {
			t.Fatalf("conclusion %q must fail the command", conclusion)
		}
		if !strings.Contains(err.Error(), conclusion) {
			t.Errorf("error must name the conclusion %q, got: %v", conclusion, err)
		}
		if !strings.Contains(err.Error(), "--log-failed") {
			t.Errorf("error must route to the logs, got: %v", err)
		}
	}
}

func TestWaitFailsWhenTheRunIsNeverIdentified(t *testing.T) {
	// GitHub never registered the run. The dispatch DID happen, so this is a lost
	// handle rather than a failed apply — and the message has to say so, or the
	// operator reads it as "the apply failed" and re-dispatches a second one on
	// top of an apply that is still running.
	stubRunLookup(t, 2, func(string, string) (Run, bool) {
		return Run{}, false
	})
	w := Watch{repo: "acme/inst", workflow: "terraform.yml", sinceID: 100, armed: true}

	err := captureStderrErr(t, w.Wait)
	if err == nil {
		t.Fatal("an unidentifiable run must fail closed — a green job here means an apply nobody watched")
	}
	if !strings.Contains(err.Error(), "lost handle") {
		t.Errorf("error must distinguish a lost handle from a failed apply, got: %v", err)
	}
}

func TestWaitFailsWhenTheBudgetIsExhausted(t *testing.T) {
	// Still running when the budget runs out. Reporting success here would be the
	// worst arm of all: the apply may yet fail, and the job that was supposed to
	// notice has already gone green.
	w := armedWatch(t)
	stubRunConclusion(t, 3, []conclusionAnswer{{status: "in_progress", ok: true}})

	err := captureStderrErr(t, w.Wait)
	if err == nil {
		t.Fatal("an unfinished run must fail the command, not pass it")
	}
	if !strings.Contains(err.Error(), "watch budget") {
		t.Errorf("error must say the budget ran out rather than blaming the run, got: %v", err)
	}
}

func TestWaitToleratesTransientlyUnreadableStatus(t *testing.T) {
	// A single unreadable poll is a blip (rate limit, transient 5xx) and must not
	// be mistaken for a verdict — but it must not spin forever either, which is
	// what the attempt budget bounds.
	w := armedWatch(t)
	stubRunConclusion(t, 10, []conclusionAnswer{
		{ok: false},
		{ok: false},
		{status: "completed", conclusion: "success", ok: true},
	})
	if err := captureStderrErr(t, w.Wait); err != nil {
		t.Fatalf("a blip before a success must not fail the command, got %v", err)
	}
}

func TestWaitOnADisarmedWatchIsAnError(t *testing.T) {
	// The belt to cmdBuild's braces. A disarmed watch followed nothing, so
	// returning nil would be a pass earned by examining no run at all.
	if err := (Watch{}).Wait(); err == nil {
		t.Error("a disarmed watch must not report success")
	}
}

func TestWaitFailsFastOnADeploymentApproval(t *testing.T) {
	// GitHub parks a run on `waiting` when an environment needs an approval, and
	// the delivered docs tell adopters to require reviewers on infra-staging /
	// infra-prod — so an unattended 04:00 dispatch lands here BY DESIGN.
	//
	// The bug this pins is not that it fails; it is WHICH failure. On the shared
	// 3h budget the message would be "did not finish within the watch budget",
	// which names the wrong problem and costs a runner three hours to say it.
	w := armedWatch(t)
	stubRunConclusion(t, 500, []conclusionAnswer{{status: "waiting", ok: true}})

	err := captureStderrErr(t, w.Wait)
	if err == nil {
		t.Fatal("a run parked on an approval must fail rather than hang")
	}
	if !strings.Contains(err.Error(), "waiting for a deployment approval") {
		t.Errorf("error must name the approval, not the budget, got: %v", err)
	}
	if strings.Contains(err.Error(), "watch budget") {
		t.Errorf("the approval case must NOT be reported as budget exhaustion: %v", err)
	}
}

func TestWaitToleratesABriefApprovalThenProceeds(t *testing.T) {
	// The short grace is what keeps this usable when a human IS watching and
	// approves promptly; without it an operator-triggered run with an approval
	// gate would fail on the first poll.
	w := armedWatch(t)
	stubRunConclusion(t, 20, []conclusionAnswer{
		{status: "waiting", ok: true},
		{status: "waiting", ok: true},
		{status: "in_progress", ok: true},
		{status: "completed", conclusion: "success", ok: true},
	})
	if err := captureStderrErr(t, w.Wait); err != nil {
		t.Fatalf("an approval given during the grace must not fail the command, got %v", err)
	}
}
