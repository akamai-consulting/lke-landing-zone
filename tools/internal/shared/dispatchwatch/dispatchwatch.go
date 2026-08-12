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
)

// LatestDispatchRun returns the newest workflow_dispatch run of workflow.
// Seamed for tests; ok=false whenever the answer cannot be obtained.
var LatestDispatchRun = func(repo, workflow string) (Run, bool) {
	var resp struct {
		Runs []struct {
			ID     uint64 `json:"id"`
			URL    string `json:"html_url"`
			Status string `json:"status"`
		} `json:"workflow_runs"`
	}
	// per_page=1: only the newest run can be the one just requested. Scoped to
	// workflow_dispatch so a push-triggered run on the same workflow is never
	// mistaken for it.
	path := fmt.Sprintf("repos/%s/actions/workflows/%s/runs?event=workflow_dispatch&per_page=1", repo, workflow)
	if err := ghapi.GHAPIJSON(path, &resp); err != nil || len(resp.Runs) == 0 {
		return Run{}, false
	}
	r := resp.Runs[0]
	if r.URL == "" {
		return Run{}, false
	}
	return Run{ID: r.ID, URL: r.URL, Status: r.Status}, true
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
)

// Watch carries what must be known BEFORE a dispatch to identify the run
// it creates afterwards.
type Watch struct {
	repo     string
	workflow string
	sinceID  uint64 // newest run before the dispatch; 0 = unknown
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
	repo, err := answers.ResolveInstanceRepo("", false)
	if err != nil {
		return Watch{}
	}
	w := Watch{repo: repo, workflow: workflow, armed: true}
	if run, ok := LatestDispatchRun(repo, workflow); ok {
		w.sinceID = run.ID
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
	if w.sinceID != 0 {
		return run.ID > w.sinceID
	}
	return run.Status != "completed"
}

// resolve polls until the run the dispatch created can be identified. ok=false
// means GitHub had not registered it within the budget — which is NOT the same
// as "it failed", and the two callers below treat it differently.
func (w Watch) resolve() (Run, bool) {
	for i := 0; i < runPollAttempts; i++ {
		sleepFn(runPollDelay)
		run, ok := LatestDispatchRun(w.repo, w.workflow)
		if ok && w.isOurs(run) {
			return run, true
		}
	}
	return Run{}, false
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
	if run, ok := w.resolve(); ok {
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
	run, ok := w.resolve()
	if !ok {
		return fmt.Errorf("--watch: dispatched %s but GitHub never registered the run, so its result is "+
			"unknown — check `gh run list --repo %s --workflow %s --limit 5`. The dispatch DID happen; "+
			"this is a lost handle, not a failed apply", w.workflow, w.repo, w.workflow)
	}
	fmt.Fprintf(os.Stderr, "%s following run %d — %s\n", color.Dim("watch:"), run.ID, color.Cyan(run.URL))

	for i := 0; i < runWatchAttempts; i++ {
		status, conclusion, ok := runConclusion(w.repo, run.ID)
		if !ok {
			// One unreadable poll is a blip (rate limit, transient 5xx); the loop
			// simply tries again and the attempt budget bounds how long that lasts.
			sleepFn(runWatchInterval)
			continue
		}
		if status != "completed" {
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
