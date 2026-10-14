package cli

// build_watch.go — give the operator a handle on the run `llz build` just
// dispatched.
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
// green, and the operator concludes the build succeeded. A fixed sleep before
// querying is a hope rather than a check.
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

// dispatchedRun is the run a dispatch produced.
type dispatchedRun struct {
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

// latestDispatchRun returns the newest workflow_dispatch run of workflow.
// Seamed for tests; ok=false whenever the answer cannot be obtained.
var latestDispatchRun = func(repo, workflow string) (dispatchedRun, bool) {
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
		return dispatchedRun{}, false
	}
	r := resp.Runs[0]
	if r.URL == "" {
		return dispatchedRun{}, false
	}
	return dispatchedRun{ID: r.ID, URL: r.URL, Status: r.Status}, true
}

// dispatchWatch carries what must be known BEFORE a dispatch to identify the run
// it creates afterwards.
type dispatchWatch struct {
	repo     string
	workflow string
	sinceID  uint64 // newest run before the dispatch; 0 = unknown
	armed    bool
}

// beginDispatchWatch records the pre-dispatch baseline. Call it before the
// dispatch; call report() after.
//
// Disarmed in the modes that dispatch nothing (--dry-run, no --yes) and whenever
// the repo or `gh` cannot be resolved, so it costs an unrelated invocation
// nothing.
func beginDispatchWatch(g globalOpts, workflow string) dispatchWatch {
	if g.DryRun || !g.Yes || !kubectlprobe.Lookable("gh") {
		return dispatchWatch{}
	}
	repo, err := answers.ResolveInstanceRepo("", false)
	if err != nil {
		return dispatchWatch{}
	}
	w := dispatchWatch{repo: repo, workflow: workflow, armed: true}
	if run, ok := latestDispatchRun(repo, workflow); ok {
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
func (w dispatchWatch) isOurs(run dispatchedRun) bool {
	if w.sinceID != 0 {
		return run.ID > w.sinceID
	}
	return run.Status != "completed"
}

// report resolves the run the dispatch created and prints where it is and how to
// follow it. Prints a route even when the run cannot be identified, because "I
// dispatched something and have no idea where it went" is the state this exists
// to remove.
func (w dispatchWatch) report() {
	if !w.armed {
		return
	}
	fmt.Fprintln(os.Stderr)
	for i := 0; i < runPollAttempts; i++ {
		sleepFn(runPollDelay)
		run, ok := latestDispatchRun(w.repo, w.workflow)
		if !ok || !w.isOurs(run) {
			continue
		}
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

// sleepFn is time.Sleep, seamed so tests don't wait.
var sleepFn = time.Sleep
