package dispatchwatch

// dispatchwatch.go — give the caller a handle on the run a dispatch just
// created: a place to look (Report), or a verdict to exit on (Wait).
//
// `gh workflow run` is fire-and-forget: it prints "Created workflow_dispatch
// event" and exits, with no run id, because GitHub creates the run
// asynchronously. So the single longest and most expensive step of the whole
// quickstart — a ~40-minute apply that creates a cluster, a VPC, buckets, and
// credentials — was the one step with nothing to watch and nothing to link.
// `gh run` appeared nowhere in this CLI. An operator who followed the quickstart
// literally was told, next, to run `llz status <env> --wait`, which cannot
// succeed until the cluster exists AND a kubeconfig is fetched AND the
// control-plane ACL admits them.
//
// It matters twice over. On the happy path it is the difference between watching
// progress and refreshing a browser tab; on the unhappy path the run URL IS the
// recovery path, because everything that explains a failed build is in that log.
//
// IDENTIFYING THE RIGHT RUN IS THE WHOLE PROBLEM. "Newest workflow_dispatch run"
// is not the run we just asked for — GitHub registers it asynchronously, so for
// the first seconds that query answers with the PREVIOUS one, which on an
// established instance is a completed run from days ago. Printing that is worse
// than printing nothing: `gh run watch` on a finished run returns instantly
// green, and the operator concludes the build succeeded. The first cut of this
// file described that hazard in a comment and then guarded it with a single
// three-second sleep, which is a hope rather than a check.
//
// So the run id observed BEFORE the dispatch is the baseline, and only a run
// with a HIGHER id is ours. Linode-style monotonic ids make that exact rather
// than heuristic; where the baseline could not be read, a completed run is
// rejected on status instead, which is weaker but still cannot hand back a
// week-old success.
//
// Best-effort throughout: the dispatch has already happened by the time any of
// this runs, so nothing here may turn a successful build into a failed command.

import (
	"fmt"
	"os"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghapi"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// Run is the run a dispatch produced.
type Run struct {
	ID     uint64
	URL    string
	Status string
}

// runPollDelay is the wait between attempts to see the new run, and
// runPollAttempts how many times to look. ~10s total: enough for GitHub to
// register a dispatch, short enough not to feel like a hang after a command
// whose whole job was to be fire-and-forget. Vars so tests neither sleep nor
// spin.
var (
	runPollDelay    = 2 * time.Second
	runPollAttempts = 5
	// waitPollAttempts is the SEPARATE, longer budget Wait() resolves under: ~90s,
	// matching release-e2e's dispatch-and-watch.sh.
	//
	// Report()'s 10s is tuned for an interactive command that has already done its
	// job and is only offering a link — giving up early there costs a convenience.
	// Wait()'s caller has no such fallback: a runs-list that lags past the budget
	// makes an unattended apply report failure while a real production apply runs
	// on, unwatched. The two callers want different answers, so they get different
	// budgets rather than one compromise that serves neither.
	waitPollAttempts = 45
)

// LatestDispatchRun returns the most recent workflow_dispatch runs of workflow,
// newest first. Seamed for tests; ok=false whenever the answer cannot be obtained.
//
// A PAGE RATHER THAN THE SINGLE NEWEST, because "newest run with an id above the
// baseline" cannot tell OUR dispatch from someone else's. The runs API carries no
// dispatch inputs, so there is no field to match a deployment on — but if two new
// runs of terraform.yml appeared since the baseline, that is knowable, and
// knowing it is the difference between reporting the wrong deployment's
// conclusion and saying we cannot tell.
var LatestDispatchRun = func(repo, workflow string) ([]Run, bool) {
	var resp struct {
		Runs []struct {
			ID     uint64 `json:"id"`
			URL    string `json:"html_url"`
			Status string `json:"status"`
		} `json:"workflow_runs"`
	}
	// Scoped to workflow_dispatch so a push-triggered run on the same workflow is
	// never mistaken for one. per_page=5 is enough to notice a second dispatch
	// without paging.
	path := fmt.Sprintf("repos/%s/actions/workflows/%s/runs?event=workflow_dispatch&per_page=5", repo, workflow)
	if err := ghapi.GHAPIJSON(path, &resp); err != nil || len(resp.Runs) == 0 {
		return nil, false
	}
	out := make([]Run, 0, len(resp.Runs))
	for _, r := range resp.Runs {
		if r.URL == "" {
			continue
		}
		out = append(out, Run{ID: r.ID, URL: r.URL, Status: r.Status})
	}
	// ok=true FOR AN EMPTY LIST. "this workflow has never been dispatched" is an
	// answer; only an unreachable API is a failure. Collapsing the two made a
	// first-ever dispatch — and every instance that has only ever applied through
	// promote.yml — indistinguishable from a broken query, which Wait() then
	// refuses to proceed on.
	return out, true
}

// runConclusion reads a run's terminal state. Seamed for tests; ok=false
// whenever the answer cannot be obtained, which wait() treats as "could not
// tell" rather than as a result.
var runConclusion = func(repo string, id uint64) (status, conclusion string, ok bool) {
	var resp struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	if err := ghapi.GHAPIJSON(fmt.Sprintf("repos/%s/actions/runs/%d", repo, id), &resp); err != nil {
		return "", "", false
	}
	return resp.Status, resp.Conclusion, true
}

// runWatchInterval is the gap between polls of a run being followed, and
// runWatchAttempts how many times to poll before giving up.
//
// BOUNDED BY ATTEMPTS, NOT WALL-CLOCK, and that is deliberate: sleepFn is a seam,
// so a test that replaces it with a no-op would spin a wall-clock loop forever
// while a `time.Now()` deadline never arrived. ~3h at 30s — the same cap
// release-e2e's dispatch-and-watch.sh uses, and comfortably past the ~40-minute
// apply plus the converge gate that follows it.
var (
	runWatchInterval = 30 * time.Second
	runWatchAttempts = 360
	// runWaitingAttempts bounds the SEPARATE budget for a run parked on a
	// deployment approval — ~5 minutes. Long enough for a human already watching
	// to click approve, short enough that a 04:00 cron fails with the right
	// diagnosis instead of holding a runner for three hours to reach the wrong one.
	runWaitingAttempts = 10
)

// Watch carries what must be known BEFORE a dispatch to identify the run
// it creates afterwards.
type Watch struct {
	repo     string
	workflow string
	sinceID  uint64 // highest run id before the dispatch; 0 with baseline=true means none existed
	// baseline records that the pre-dispatch lookup SUCCEEDED. It is not the same
	// as sinceID != 0: a workflow dispatched for the first time legitimately has a
	// baseline of zero, and treating that as "could not read the run list" turns a
	// correct first apply into a red job with a misdiagnosing message.
	baseline bool
	armed    bool
}

// Begin records the pre-dispatch baseline. Call it before the dispatch; call
// Report() or Wait() after.
//
// Takes the two flags rather than the CLI's globalOpts: this package is reached
// from the command layer, not part of it (ADR 0013/0014), and a dependency on a
// CLI struct is what would drag it back.
//
// Disarmed in the modes that dispatch nothing (--dry-run, no --yes) and whenever
// the repo or `gh` cannot be resolved, so it costs an unrelated invocation
// nothing.
func Begin(dryRun, yes bool, workflow string) Watch {
	if dryRun || !yes || !kubectlprobe.Lookable("gh") {
		return Watch{}
	}
	// GH_REPO FIRST, because that is what the dispatch itself targets — `gh`
	// honours it over anything in the checkout. The pin in .copier-answers.yml is
	// the fallback, and the two diverge exactly when a repo has been renamed or
	// transferred: the dispatch then lands in the new repo while the watcher polls
	// the old name. Report() only lost a link to that; Wait() would fail an armed
	// scheduled apply, or worse, follow an unrelated run in a repo that still
	// answers to the old name.
	repo := os.Getenv("GH_REPO")
	if repo == "" {
		r, err := answers.ResolveInstanceRepo("", false)
		if err != nil {
			return Watch{}
		}
		repo = r
	}
	w := Watch{repo: repo, workflow: workflow, armed: true}
	// The HIGHEST id present, not runs[0]: the API returns newest-first, but the
	// baseline's whole job is to be an upper bound on "runs that existed before we
	// dispatched", and taking the max is what makes that true regardless of order.
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

// isOurs reports whether run is plausibly the one the dispatch just created.
//
// With a baseline the test is exact (ids increase monotonically). Without one —
// the pre-dispatch lookup failed — fall back to rejecting a COMPLETED run: a run
// that finished before we dispatched cannot be ours, and that single check is
// what stops the "watch a week-old green run" failure. A queued/in_progress run
// that predates the dispatch by seconds can still slip through, which is
// acceptable: it is a live run of the same workflow on the same deployment.
func (w Watch) isOurs(run Run) bool {
	if w.baseline {
		return run.ID > w.sinceID
	}
	return run.Status != "completed"
}

// resolve polls until the run the dispatch created can be identified. ok=false
// means GitHub had not registered it within the budget — which is NOT the same
// as "it failed", and the two callers below treat it differently.
// resolve polls until exactly one candidate run can be identified.
//
// ambiguous=true means more than one workflow_dispatch run appeared after the
// baseline — someone dispatched the same workflow (a different deployment, most
// likely) inside the window. There is no field to tell them apart, so the honest
// answer is that we cannot, and Wait() must say so rather than follow whichever
// one sorted first and report ITS conclusion for OUR apply.
func (w Watch) resolve(attempts int) (run Run, found, ambiguous bool) {
	for i := 0; i < attempts; i++ {
		sleepFn(runPollDelay)
		runs, ok := LatestDispatchRun(w.repo, w.workflow)
		if !ok {
			continue
		}
		var ours []Run
		for _, r := range runs {
			if w.isOurs(r) {
				ours = append(ours, r)
			}
		}
		switch {
		case len(ours) == 1:
			return ours[0], true, false
		case len(ours) > 1 && w.baseline:
			// Only meaningful WITH a baseline: without one, isOurs falls back to
			// "not completed", which legitimately matches several live runs and
			// says nothing about who started them.
			return Run{}, false, true
		}
	}
	return Run{}, false, false
}

// report resolves the run the dispatch created and prints where it is and how to
// follow it. Prints a route even when the run cannot be identified, because "I
// dispatched something and have no idea where it went" is the state this exists
// to remove.
func (w Watch) Report() {
	if !w.armed {
		return
	}
	fmt.Fprintln(os.Stderr)
	if run, ok, _ := w.resolve(runPollAttempts); ok {
		fmt.Fprintf(os.Stderr, "%s dispatched — the build runs in GitHub Actions and takes ~40 minutes.\n", color.Green("✓"))
		fmt.Fprintf(os.Stderr, "    %s %s\n", color.Dim("run:"), color.Cyan(run.URL))
		fmt.Fprintf(os.Stderr, "    %s %s\n", color.Dim("follow:"), color.Cyan(fmt.Sprintf("gh run watch %d --repo %s", run.ID, w.repo)))
		// Named here rather than only in a runbook: this is the moment an operator
		// learns the run failed, and the moment they need to know where to look.
		fmt.Fprintf(os.Stderr, "    %s %s\n", color.Dim("if it fails:"), color.Cyan(fmt.Sprintf("gh run view %d --repo %s --log-failed", run.ID, w.repo)))
		fmt.Fprintln(os.Stderr, color.Dim("                 then docs/runbooks/first-build-failed.md — the apply is re-runnable"))
		return
	}
	// Deliberately does NOT fall back to "the newest run": that is the wrong-run
	// bug this whole file is built around. A command the operator runs themselves
	// is the honest answer when we cannot identify ours.
	fmt.Fprintf(os.Stderr, "%s dispatched. The run takes ~40 minutes; GitHub had not registered it yet, so find it with:\n", color.Green("✓"))
	fmt.Fprintf(os.Stderr, "    %s\n", color.Cyan("gh run list --repo "+w.repo+" --workflow "+w.workflow+" --limit 5"))
	fmt.Fprintln(os.Stderr, color.Dim("    if it fails: docs/runbooks/first-build-failed.md — the apply is re-runnable"))
}

// wait follows the dispatched run to completion and returns nil only if it
// SUCCEEDED. It is what makes `llz build --watch` usable as a CI step: report()
// is best-effort by design (the dispatch already happened, so nothing it does may
// fail the command), and an unattended caller needs the opposite contract.
//
// FAILS CLOSED ON EVERY "COULD NOT TELL". A scheduled apply whose job goes green
// because the watcher lost track of the run is precisely the shape this repo
// keeps re-learning: an absence of evidence rendered as a passing check. So an
// unarmed watch, an unidentifiable run, an unreadable status and an exhausted
// budget are all errors — distinct ones, because they have nothing in common as
// remedies.
func (w Watch) Wait() error {
	if !w.armed {
		// Unreachable from `--watch --yes` (cmdBuild rejects the combinations that
		// disarm it before dispatching), so this is the belt to that braces: a
		// future caller that arms nothing must not silently get a pass.
		return fmt.Errorf("--watch cannot follow the run: the dispatch watcher was never armed " +
			"(needs --yes, a resolvable instance repo, and `gh` on PATH)")
	}
	// NO BASELINE, NO VERDICT. When the pre-dispatch lookup FAILED, both of
	// resolve's safeguards
	// collapse at once: isOurs degrades to "not completed", which matches ANY live
	// run, and the ambiguity guard is disabled because it cannot mean anything
	// without an ordering. Wait() would then be free to follow a stranger's run and
	// return nil on ITS success — a fail-open in the one function whose entire
	// contract is to fail closed. Report() may still guess, because the worst it
	// can do is print a link to the wrong page.
	if !w.baseline {
		return fmt.Errorf("--watch could not read the run list before dispatching, so it has no baseline to tell "+
			"this run from any other — refusing to report an unrelated run's result as this apply's.\n"+
			"    the dispatch DID happen: gh run list --repo %s --workflow %s --limit 5", w.repo, w.workflow)
	}
	run, ok, ambiguous := w.resolve(waitPollAttempts)
	if ambiguous {
		return fmt.Errorf("--watch: more than one %s run was dispatched while this one was starting, and the runs API "+
			"carries no inputs to tell them apart — refusing to report another deployment's conclusion as this one's.\n"+
			"    check the runs yourself: gh run list --repo %s --workflow %s --limit 5", w.workflow, w.repo, w.workflow)
	}
	if !ok {
		return fmt.Errorf("--watch: dispatched %s but GitHub never registered the run, so its result is "+
			"unknown — check `gh run list --repo %s --workflow %s --limit 5`. The dispatch DID happen; "+
			"this is a lost handle, not a failed apply", w.workflow, w.repo, w.workflow)
	}
	fmt.Fprintf(os.Stderr, "%s following run %d — %s\n", color.Dim("watch:"), run.ID, color.Cyan(run.URL))

	waiting := 0
	for i := 0; i < runWatchAttempts; i++ {
		status, conclusion, ok := runConclusion(w.repo, run.ID)
		if !ok {
			// One unreadable poll is a blip (rate limit, transient 5xx); the loop
			// simply tries again and the attempt budget bounds how long that lasts.
			sleepFn(runWatchInterval)
			continue
		}
		// `waiting` is GitHub's status for a run parked on an environment approval,
		// and it needs its own, much shorter budget. The docs tell adopters to put
		// required reviewers on infra-staging / infra-prod, so an unattended 04:00
		// dispatch parks there by design — and spending the full 3h attempt budget
		// on it would report "did not finish within the watch budget", which names
		// the wrong problem entirely and costs a runner three hours to say it.
		// The short grace still covers someone approving promptly.
		if status == "waiting" {
			waiting++
			if waiting > runWaitingAttempts {
				return fmt.Errorf("run %d is waiting for a deployment approval and no one has given it — %s\n"+
					"    an unattended dispatch cannot approve itself. Either approve the run, or remove the required\n"+
					"    reviewers from the environment this deployment applies into if it is meant to run on a timer",
					run.ID, run.URL)
			}
			sleepFn(runWatchInterval)
			continue
		}
		if status != "completed" {
			// RESET, because a `module=all` chain parks more than once: apply-vpc,
			// apply-cluster, apply-object-storage and bootstrap-openbao each carry
			// their own `environment:`. A cumulative counter turned the grace into a
			// budget across the whole run, so a chain whose approvals were all GIVEN
			// still failed with "no one has given it" — blaming a human who had.
			waiting = 0
			sleepFn(runWatchInterval)
			continue
		}
		if conclusion == "success" {
			fmt.Fprintf(os.Stderr, "%s run %d succeeded\n", color.Green("✓"), run.ID)
			return nil
		}
		// Names the conclusion rather than collapsing to "failed": cancelled,
		// timed_out and startup_failure send the reader to three different places,
		// and startup_failure in particular produces no annotations at all — the
		// failure mode that left a delivered rotation workflow dead for months.
		return fmt.Errorf("run %d concluded %q — %s\n    logs: gh run view %d --repo %s --log-failed",
			run.ID, conclusion, run.URL, run.ID, w.repo)
	}
	return fmt.Errorf("run %d did not finish within the watch budget (%d polls at %s) and is still going — %s",
		run.ID, runWatchAttempts, runWatchInterval, run.URL)
}

// sleepFn is time.Sleep, seamed so tests don't wait.
var sleepFn = time.Sleep
