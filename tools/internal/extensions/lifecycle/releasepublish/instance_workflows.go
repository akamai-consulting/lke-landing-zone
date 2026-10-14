package releasepublish

// instance_workflows.go implements `llz ci assert-instance-workflow` — the
// release-e2e lane's proof that a delivered DAY-2 workflow runs on a real instance
// and does its job, rather than being a caller stub nothing has ever executed.
//
// WHY IT EXISTS (#421). Until this verb the lane drove exactly one delivered
// workflow, terraform.yml. scheduled-checks.yml and secret-rotation.yml are cron
// entry points that nothing in the template repo ever ran — and the caller stub is
// precisely where the failure lands. secret-rotation.yml produced `startup_failure`
// on a live adopter every night for months: the caller job held `contents: read`
// while a callee job asked for `id-token: write`, so GitHub refused to start the
// run. No jobs, no annotations, no notification, no logs. The static gate
// (reusable-workflow-caller-permissions) now holds that one shape; it cannot see a
// `workflow_call` input that no longer matches, a composite action that fails at
// runtime, an expression that evaluates to the wrong thing, or a verb that resolves
// and then errors against real state. Only running the workflow sees those.
//
// WHAT "DID ITS JOB" MEANS, AND WHY A GREEN RUN IS NOT IT. Every cluster-touching
// job in these workflows takes its kubeconfig through `cluster-access` with
// `allow-missing: 'true'`, and gates its checks on `steps.kubeconfig.outputs
// .available == 'true'`. That is correct for an adopter — a torn-down deployment
// is not an incident — and it means a run whose cluster access silently failed is
// GREEN: every check skips and every job succeeds having examined nothing. So the
// verdict is read at the consumer, per STEP: each profile names the checks that
// must have RUN (conclusion success, not skipped) in each job, for each deployment
// the caller says has a cluster. The expected set is hardcoded in the profile and
// passed in by the caller, never derived from the run — a run that lost a job or a
// matrix leg would otherwise yield a smaller expected set and pass.
//
// THE ROTATION PROFILES ARE DRY RUNS, and the fence is checked twice. Running the
// rotation for real in the release gate would mint and revoke ACCOUNT-scoped
// Linode PATs and TF-state OBJ keys on an account shared with other lanes. Every
// `*-apply` input is passed as an explicit `false`, the coupling test feeds those
// inputs through the real rotation router (tokeninv.RouteOutputs) and fails if any
// mutation would be armed, and at runtime any job on the profile's mustNotRun list
// that actually executed fails the verb — a dry-run dispatch that ran a mutating
// job is the one outcome worse than a red lane.
//
// I/O sits behind package-var seams (wfGH / wfSleep / wfNow) so the dispatch, the
// run lookup, the wait and the verdict are all tested without a forge.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ── Profiles ─────────────────────────────────────────────────────────────────

// dispatchInput is one `-f key=value` on the dispatch. Ordered (a slice, not a
// map) so the argv is deterministic and the tests can assert it.
type dispatchInput struct{ Key, Value string }

// expectedJob is a delivered job that must appear AND succeed, with the steps
// inside it that must have RUN. Name may carry {deployment}, which expands once
// per deployment the caller names — the matrix legs that must exist.
type expectedJob struct {
	Name  string
	Steps []string
	// Source is the delivered workflow file that declares the job, when it is not
	// the profile's Body — `Discover deployments` lives in the shared
	// llz-discover-deployments.yml that both bodies call. Read only by the
	// coupling test, which checks every name here against the delivered YAML.
	Source string
}

// workflowProfile is everything the verb needs to dispatch one delivered entry
// point and judge the run. It is the single place a profile is written: the lane
// names it, this verb dispatches and judges it, the coupling test checks it
// against the delivered YAML and the rotation router, and the coverage ledger
// (internal/cli) reads it to decide what counts as DRIVEN.
type workflowProfile struct {
	Name     string // what the lane passes to --profile
	Workflow string // the caller stub dispatched (instance-template/.github/workflows/)
	Body     string // the reusable body the stub calls — the coupling test reads it
	Inputs   []dispatchInput
	Jobs     []expectedJob
	// MustNotRun are jobs that must be absent or skipped. A dry-run dispatch that
	// executed one of these mutated something it was promised not to.
	MustNotRun []string
	// RouteOutput is, for a rotation profile, the `run-*` output of the rotation
	// router that gates the profile's one working job. The coupling test asserts
	// it is the ONLY run-* the router turns on for these inputs, and that the job
	// really is gated on it.
	RouteOutput string
	Timeout     time.Duration // default wait for this workflow's run to complete
}

// The deployment placeholder in a job name, and the one shared job.
const (
	deploymentToken = "{deployment}"
	discoverSource  = "llz-discover-deployments.yml"
)

var discoverJob = expectedJob{Name: "Discover deployments", Steps: []string{"List deployments"}, Source: discoverSource}

// rotationReason is the audit-log reason every e2e rotation dispatch carries. The
// router refuses a blank one, and it is what an operator reading the fixture's
// run list sees, so it says what the run is and that it changed nothing.
const rotationReason = "release-e2e dry run of the delivered rotation caller - no credential is minted or revoked"

// rotationMustNotRun are the rotation jobs that mutate whatever their inputs say:
// lke-admin deletes the cluster's kubeconfig, propagate mints an in-cluster PAT,
// state-passphrase re-keys every root, db-admin resets a database password in
// place. None of them has a dry-run mode, so none may run from the release gate.
var rotationMustNotRun = []string{
	"Rotate lke-admin ({deployment})",
	"Rotate the in-cluster PAT on {deployment}",
	"Rotate state-encryption passphrase",
	"Rotate Managed Postgres admin ({deployment})",
}

// rotationProfile builds the dry-run profile for one rotation scope. Every arming
// input the stub declares is passed as an explicit false: the stub defaults them
// to false today, and a default is exactly the kind of value that changes
// without anyone re-reading the e2e that depended on it.
func rotationProfile(scope, job, step, routeOutput string) workflowProfile {
	return workflowProfile{
		Name:     "rotation-" + scope,
		Workflow: "secret-rotation.yml",
		Body:     "llz-secret-rotation.yml",
		Inputs: []dispatchInput{
			{"scope", scope},
			{"confirm", "rotate:" + scope},
			{"reason", rotationReason},
			{"pat-apply", "false"},
			{"revoke-apply", "false"},
			{"tf-state-apply", "false"},
			{"tf-state-revoke-apply", "false"},
			{"db-admin-apply", "false"},
		},
		Jobs: []expectedJob{
			discoverJob,
			{Name: "Setup", Steps: []string{"Route scope + validate emergency confirmation"}},
			{Name: job, Steps: []string{step}},
		},
		MustNotRun:  rotationMustNotRun,
		RouteOutput: routeOutput,
		Timeout:     20 * time.Minute,
	}
}

// instanceWorkflowProfiles is the table. Adding a profile is not enough to drive
// a workflow — the lane has to invoke it, and TestEveryProfileIsInvokedByTheLane
// fails until it does.
var instanceWorkflowProfiles = []workflowProfile{
	{
		Name:     "scheduled-checks",
		Workflow: "scheduled-checks.yml",
		Body:     "llz-scheduled-checks.yml",
		// The stub's own defaults (drift against main, report-only), spelled out
		// so the dispatch never depends on how gh treats an input-less dispatch.
		// On workflow_dispatch EVERY job runs — the crons each select a subset,
		// the dispatch selects them all, which is what makes one run cover the
		// whole caller.
		Inputs: []dispatchInput{{"drift_branch", "main"}, {"drift_strict", "false"}},
		Jobs: []expectedJob{
			discoverJob,
			{Name: "Weekly cluster checks ({deployment})", Steps: []string{
				"Check OpenBao seal + ESO readiness",
				"Check cert-manager Certificate readiness",
			}},
			{Name: "lke-admin Rotation SLA ({deployment})", Steps: []string{
				"Check lke-admin-token age",
			}},
			{Name: "App-scope convergence ({deployment})", Steps: []string{
				"Converge the instance-owned content",
			}},
			{Name: "Credential Single Pane ({deployment})", Steps: []string{
				"Write token inventory to the cluster",
				"Assert the credential funnel end to end",
				"Read cluster credential alert status",
			}},
			{Name: "PrometheusRule Health ({deployment})", Steps: []string{
				"Check PrometheusRule CRs present",
				"Check Prometheus has loaded rules (no evaluation errors)",
			}},
			{Name: "Template drift", Steps: []string{
				"Compare the instance's template pin against the template head",
			}},
		},
		// The slowest job (Weekly cluster checks) caps itself at 20 minutes, and the
		// credential single pane settles for up to 5 minutes twice; 45 covers a full
		// run plus queueing on a shared runner pool.
		Timeout: 45 * time.Minute,
	},
	rotationProfile("linode-pat", "Create LINODE_API_TOKEN",
		"Rotate PAT + write the new token to each deployment's env secret", "run-pat-create"),
	rotationProfile("linode-pat-revoke", "Revoke old LINODE_API_TOKEN PATs",
		"Revoke old PATs", "run-pat-revoke"),
	rotationProfile("tf-state-key", "Create TF_STATE_ACCESS_KEY pair",
		"Rotate OBJ key + write TF_STATE_* to each deployment's env secrets", "run-tf-state-create"),
	rotationProfile("tf-state-key-revoke", "Revoke old TF_STATE_ACCESS_KEY pairs",
		"Revoke old OBJ keys", "run-tf-state-revoke"),
}

func lookupProfile(name string) (workflowProfile, bool) {
	for _, p := range instanceWorkflowProfiles {
		if p.Name == name {
			return p, true
		}
	}
	return workflowProfile{}, false
}

func profileNames() []string {
	names := make([]string, 0, len(instanceWorkflowProfiles))
	for _, p := range instanceWorkflowProfiles {
		names = append(names, p.Name)
	}
	return names
}

// InstanceWorkflowProfile is the exported view of a profile, for the delivered
// entry-point coverage ledger in internal/cli: a workflow counts as DRIVEN by the
// release lane when a profile dispatches it AND the lane invokes that profile.
type InstanceWorkflowProfile struct{ Name, Workflow string }

// InstanceWorkflowProfiles lists every profile the verb can dispatch.
func InstanceWorkflowProfiles() []InstanceWorkflowProfile {
	out := make([]InstanceWorkflowProfile, 0, len(instanceWorkflowProfiles))
	for _, p := range instanceWorkflowProfiles {
		out = append(out, InstanceWorkflowProfile{Name: p.Name, Workflow: p.Workflow})
	}
	return out
}

// ── Seams ────────────────────────────────────────────────────────────────────

var (
	// wfGH runs `gh <args>` against host with GH_TOKEN set. Same contract as
	// gatesGH: stdout comes back, stderr is folded into the error.
	wfGH = func(token, host string, args ...string) ([]byte, error) {
		cmd := exec.Command("gh", args...)
		cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
		if host != "" {
			cmd.Env = append(cmd.Env, "GH_HOST="+host)
		}
		out, err := cmd.Output()
		return out, ghError(args, err)
	}
	wfSleep = func(d time.Duration) { time.Sleep(d) }
	wfNow   = time.Now
)

// ── Options ──────────────────────────────────────────────────────────────────

// InstanceWorkflowOpts is the verb's input.
type InstanceWorkflowOpts struct {
	Instance    string   // owner/name of the throwaway instance repo
	Host        string   // forge host (github.com, or a GHES appliance)
	Ref         string   // branch to dispatch on (the instance's default branch)
	Token       string   // actions:read/write on the instance repo
	Profile     string   // which profile to dispatch and judge
	Deployments []string // the deployments that HAVE a cluster — each must yield its matrix legs
	Interval    time.Duration
	Timeout     time.Duration // 0 = the profile's own
	// RunID, when set, JUDGES AN EXISTING RUN instead of dispatching a new one —
	// for an operator re-reading a red lane's run, or checking what the verdict
	// would have been on a run that already happened (a scheduled one included,
	// as long as it selected the profile's jobs). Nothing is dispatched.
	RunID int64
}

// ── The run as gh reports it ─────────────────────────────────────────────────

type runStep struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type runJob struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	URL        string    `json:"url"`
	Steps      []runStep `json:"steps"`
}

// runView is `gh run view <id> --json status,conclusion,url,jobs`.
type runView struct {
	Status     string   `json:"status"`
	Conclusion string   `json:"conclusion"`
	URL        string   `json:"url"`
	Jobs       []runJob `json:"jobs"`
}

// ── Verdict (pure) ───────────────────────────────────────────────────────────

// expandName substitutes a deployment into a job-name template.
func expandName(tmpl, deployment string) string {
	return strings.ReplaceAll(tmpl, deploymentToken, deployment)
}

// jobMatches reports whether an observed job name is the delivered job `want`.
// A job inside a called workflow is reported as "<caller job> / <job>", and one
// level deeper again for the discover workflow both bodies call — so the match is
// on the whole name or on a " / "-separated suffix, never on a substring
// ("Setup" must not match "Setup kubectl").
func jobMatches(observed, want string) bool {
	return observed == want || strings.HasSuffix(observed, " / "+want)
}

func findJob(jobs []runJob, want string) (runJob, bool) {
	for _, j := range jobs {
		if jobMatches(j.Name, want) {
			return j, true
		}
	}
	return runJob{}, false
}

// failedConclusions are the job conclusions that are a verdict against the run.
// skipped and success are not; neutral is not produced by Actions jobs.
var failedConclusions = map[string]bool{
	"failure": true, "cancelled": true, "timed_out": true, "action_required": true, "startup_failure": true,
}

// evaluateWorkflowRun is the whole judgement, over a completed run. It returns
// every problem found rather than the first, so one red lane names everything
// that is wrong with the run instead of costing one release per finding.
func evaluateWorkflowRun(p workflowProfile, deployments []string, v runView) []string {
	// FIRST, because it voids everything else: GitHub refused to start the run,
	// so there are no jobs to read. Named specifically because the bare symptom
	// — a run with no jobs — sends a reader to the jobs, and there are none.
	if v.Conclusion == "startup_failure" {
		return []string{fmt.Sprintf("%s concluded startup_failure: GitHub refused to START the run, so no job "+
			"executed and nothing was logged. This is the failure mode the caller stub exists to avoid — check, "+
			"in order: (1) the caller job's permissions are at least the union of every called job's (a called "+
			"workflow's jobs can never hold more than the calling job's token; `llz ci "+
			"reusable-workflow-caller-permissions` gates this statically), (2) every input the stub passes is "+
			"declared by %s's workflow_call, (3) the vendored workflows parse. Run: %s", p.Workflow, p.Body, v.URL)}
	}
	if len(v.Jobs) == 0 {
		return []string{fmt.Sprintf("%s ran no jobs at all (conclusion %q) — a run that examined nothing is not a "+
			"pass. Run: %s", p.Workflow, v.Conclusion, v.URL)}
	}

	var problems []string
	expected := map[string]bool{}
	for _, ej := range p.Jobs {
		names := []string{ej.Name}
		if strings.Contains(ej.Name, deploymentToken) {
			names = names[:0]
			for _, d := range deployments {
				names = append(names, expandName(ej.Name, d))
			}
		}
		for _, name := range names {
			j, ok := findJob(v.Jobs, name)
			if !ok {
				problems = append(problems, fmt.Sprintf("job %q never appeared in the run — if it is a matrix leg, "+
					"the discover job did not emit that deployment; if not, the job was renamed or removed "+
					"(TestInstanceWorkflowProfilesMatchTheDeliveredWorkflows fails on a rename first)", name))
				continue
			}
			expected[j.Name] = true
			if j.Conclusion != "success" {
				problems = append(problems, fmt.Sprintf("job %q concluded %q, not success — %s", j.Name, orNone(j.Conclusion), j.URL))
				continue
			}
			problems = append(problems, missingSteps(j, ej.Steps)...)
		}
	}

	for _, tmpl := range p.MustNotRun {
		for _, name := range expandAll(tmpl, deployments) {
			j, ok := findJob(v.Jobs, name)
			if ok && j.Conclusion != "skipped" && j.Conclusion != "" {
				problems = append(problems, fmt.Sprintf("job %q EXECUTED (conclusion %q) on a dispatch that must "+
					"not run it. This profile is a DRY RUN on a shared account; a mutating job running here means "+
					"the dispatch inputs no longer route the way the coupling test last proved — %s", j.Name, j.Conclusion, j.URL))
			}
		}
	}

	// Any OTHER job that ran and failed is a delivered job failing, whatever its
	// name. Unexpected skips are fine — they are the branches this dispatch does
	// not take.
	for _, j := range v.Jobs {
		if !expected[j.Name] && failedConclusions[j.Conclusion] {
			problems = append(problems, fmt.Sprintf("job %q (not one this profile asserts) concluded %q — %s",
				j.Name, j.Conclusion, j.URL))
		}
	}

	// Catch-all, last: a run GitHub marks unsuccessful must not pass because none
	// of the rules above happened to name why.
	if len(problems) == 0 && v.Conclusion != "success" {
		problems = append(problems, fmt.Sprintf("%s concluded %q with every asserted job green — read the run: %s",
			p.Workflow, orNone(v.Conclusion), v.URL))
	}
	return problems
}

// missingSteps returns a problem for every required step that did not RUN and
// succeed. A skipped check is the vacuous green this verb exists to refuse, and
// its message says the likely reason rather than only the symptom.
func missingSteps(j runJob, want []string) []string {
	var problems []string
	for _, name := range want {
		var got *runStep
		for i := range j.Steps {
			if j.Steps[i].Name == name {
				got = &j.Steps[i]
				break
			}
		}
		switch {
		case got == nil:
			problems = append(problems, fmt.Sprintf("job %q has no step %q (steps present: %s) — renamed or removed",
				j.Name, name, stepNames(j.Steps)))
		case got.Conclusion == "skipped":
			problems = append(problems, fmt.Sprintf("job %q succeeded WITHOUT RUNNING %q — the check was skipped, "+
				"so the job examined nothing. On the cluster-touching jobs this is cluster-access reporting no "+
				"cluster (allow-missing): read that job's `Cluster access` step for why the kubeconfig was "+
				"unavailable — %s", j.Name, name, j.URL))
		case got.Conclusion != "success":
			problems = append(problems, fmt.Sprintf("job %q: step %q concluded %q — %s", j.Name, name, orNone(got.Conclusion), j.URL))
		}
	}
	return problems
}

func expandAll(tmpl string, deployments []string) []string {
	if !strings.Contains(tmpl, deploymentToken) {
		return []string{tmpl}
	}
	out := make([]string, 0, len(deployments))
	for _, d := range deployments {
		out = append(out, expandName(tmpl, d))
	}
	return out
}

func stepNames(steps []runStep) string {
	if len(steps) == 0 {
		return "none"
	}
	names := make([]string, 0, len(steps))
	for _, s := range steps {
		names = append(names, fmt.Sprintf("%q", s.Name))
	}
	return strings.Join(names, ", ")
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

// ── Dispatch, locate, wait ───────────────────────────────────────────────────

// runURLRe pulls a run id out of `gh workflow run`'s output, which newer gh
// versions print as the created run's URL.
var runURLRe = regexp.MustCompile(`/actions/runs/(\d+)`)

type listedRun struct {
	ID int64 `json:"databaseId"`
}

// dispatchRunIDs lists the ids of this workflow's recent workflow_dispatch runs.
func dispatchRunIDs(o InstanceWorkflowOpts, p workflowProfile) (map[int64]bool, error) {
	out, err := wfGH(o.Token, o.Host, "run", "list", "--repo", o.Instance, "--workflow", p.Workflow,
		"--event", "workflow_dispatch", "--limit", "20", "--json", "databaseId")
	if err != nil {
		return nil, err
	}
	var runs []listedRun
	if err := json.Unmarshal(out, &runs); err != nil {
		return nil, fmt.Errorf("parsing `gh run list` output: %w", err)
	}
	ids := map[int64]bool{}
	for _, r := range runs {
		ids[r.ID] = true
	}
	return ids, nil
}

// dispatchArgs is the `gh workflow run` argv for a profile.
func dispatchArgs(o InstanceWorkflowOpts, p workflowProfile) []string {
	args := []string{"workflow", "run", p.Workflow, "--repo", o.Instance, "--ref", o.Ref}
	for _, in := range p.Inputs {
		args = append(args, "-f", in.Key+"="+in.Value)
	}
	return args
}

// dispatchAndLocate fires the dispatch and returns the id of the run it created.
//
// THE RUN IS IDENTIFIED BY ELIMINATION, not by timestamp: the ids that existed
// before the dispatch are recorded, and the run is the newest id that was not
// among them. A createdAt comparison (what dispatch-and-watch.sh does) depends on
// two clocks agreeing; this depends on nothing. When gh prints the run URL — newer
// versions do — that is used directly.
func dispatchAndLocate(o InstanceWorkflowOpts, p workflowProfile) (int64, error) {
	before, err := dispatchRunIDs(o, p)
	if err != nil {
		return 0, fmt.Errorf("listing %s runs before dispatch: %w", p.Workflow, err)
	}

	var out []byte
	var dispatchErr error
	// A freshly force-pushed workflow can take a few seconds to become
	// dispatchable — the same allowance dispatch-and-watch.sh makes.
	for attempt := 1; attempt <= 6; attempt++ {
		out, dispatchErr = wfGH(o.Token, o.Host, dispatchArgs(o, p)...)
		if dispatchErr == nil {
			break
		}
		fmt.Printf("dispatch attempt %d of %s failed (%v) — retrying in 10s\n", attempt, p.Workflow, dispatchErr)
		wfSleep(10 * time.Second)
	}
	if dispatchErr != nil {
		return 0, fmt.Errorf("could not dispatch %s on %s: %w", p.Workflow, o.Instance, dispatchErr)
	}
	if m := runURLRe.FindSubmatch(out); m != nil {
		var id int64
		if _, err := fmt.Sscan(string(m[1]), &id); err == nil && id > 0 {
			return id, nil
		}
	}

	for attempt := 0; attempt < 45; attempt++ {
		if attempt > 0 {
			wfSleep(2 * time.Second)
		}
		after, err := dispatchRunIDs(o, p)
		if err != nil {
			continue // transient; the bound below still applies
		}
		var fresh []int64
		for id := range after {
			if !before[id] {
				fresh = append(fresh, id)
			}
		}
		if len(fresh) > 0 {
			sort.Slice(fresh, func(i, j int) bool { return fresh[i] > fresh[j] })
			return fresh[0], nil
		}
	}
	return 0, fmt.Errorf("dispatched %s on %s but no new workflow_dispatch run appeared within ~90s", p.Workflow, o.Instance)
}

// awaitRun polls the run until it completes, bounded by timeout. On timeout it
// cancels the run (best effort) so a hung day-2 workflow cannot outlive the lane
// that started it and race the teardown.
func awaitRun(o InstanceWorkflowOpts, id int64, timeout time.Duration) (runView, error) {
	deadline := wfNow().Add(timeout)
	idStr := fmt.Sprint(id)
	var lastErr error
	for {
		out, err := wfGH(o.Token, o.Host, "run", "view", idStr, "--repo", o.Instance,
			"--json", "status,conclusion,url,jobs")
		if err == nil {
			var v runView
			if perr := json.Unmarshal(out, &v); perr != nil {
				lastErr = fmt.Errorf("parsing `gh run view` output: %w", perr)
			} else if v.Status == "completed" {
				return v, nil
			}
		} else {
			lastErr = err
		}
		if !wfNow().Before(deadline) {
			_, _ = wfGH(o.Token, o.Host, "run", "cancel", idStr, "--repo", o.Instance)
			if lastErr != nil {
				return runView{}, fmt.Errorf("run %d did not complete within %s (cancelled); last read error: %w", id, timeout, lastErr)
			}
			return runView{}, fmt.Errorf("run %d did not complete within %s (cancelled)", id, timeout)
		}
		wfSleep(o.Interval)
	}
}

// ── Entry point ──────────────────────────────────────────────────────────────

// RunAssertInstanceWorkflow dispatches the profile's workflow on the instance,
// waits for it, and fails unless every asserted job and check ran and passed.
func RunAssertInstanceWorkflow(o InstanceWorkflowOpts) error {
	for _, v := range []struct{ name, val string }{
		{"--instance", o.Instance}, {"--profile", o.Profile}, {"GH_TOKEN", o.Token},
	} {
		if v.val == "" {
			return fmt.Errorf("assert-instance-workflow: %s is required", v.name)
		}
	}
	p, ok := lookupProfile(o.Profile)
	if !ok {
		return fmt.Errorf("assert-instance-workflow: unknown --profile %q (known: %s)", o.Profile, strings.Join(profileNames(), ", "))
	}
	// THE DEPLOYMENT SET IS THE CALLER'S, AND IT IS REQUIRED. A profile with
	// per-deployment jobs and no deployments would expand to no expected legs and
	// pass on a run whose matrix came back empty — exactly the vacuous green this
	// verb is for.
	var deployments []string
	for _, d := range o.Deployments {
		if d = strings.TrimSpace(d); d != "" {
			deployments = append(deployments, d)
		}
	}
	if len(deployments) == 0 {
		return fmt.Errorf("assert-instance-workflow: at least one --deployment is required — the deployments " +
			"that have a cluster, whose matrix legs must each run their checks")
	}
	o.Deployments = deployments
	if o.Host == "" {
		o.Host = "github.com"
	}
	if o.Ref == "" {
		o.Ref = "main"
	}
	if o.Interval <= 0 {
		o.Interval = 20 * time.Second
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = p.Timeout
	}

	id := o.RunID
	if id == 0 {
		fmt.Printf("Dispatching %s on %s@%s (profile %s, deployments %s)\n",
			p.Workflow, o.Instance, o.Ref, p.Name, strings.Join(deployments, ","))
		var err error
		if id, err = dispatchAndLocate(o, p); err != nil {
			return fmt.Errorf("::error title=%s was not dispatched::%w", p.Workflow, err)
		}
	} else {
		fmt.Printf("Judging existing run %d of %s on %s (profile %s, deployments %s) — nothing dispatched\n",
			id, p.Workflow, o.Instance, p.Name, strings.Join(deployments, ","))
	}
	fmt.Printf("Watching run %d (budget %s)\n", id, timeout)
	v, err := awaitRun(o, id, timeout)
	if err != nil {
		return fmt.Errorf("::error title=%s did not finish::%w", p.Workflow, err)
	}

	fmt.Printf("Run %d concluded %s: %s\n", id, orNone(v.Conclusion), v.URL)
	for _, j := range v.Jobs {
		fmt.Printf("  %-70s %s\n", j.Name, orNone(j.Conclusion))
	}
	problems := evaluateWorkflowRun(p, deployments, v)
	if len(problems) > 0 {
		for _, pr := range problems {
			fmt.Printf("::error title=%s::%s\n", p.Workflow, pr)
		}
		return fmt.Errorf("%s (profile %s): %d problem(s) — see the annotations above", p.Workflow, p.Name, len(problems))
	}
	asserted := 0
	for _, ej := range p.Jobs {
		n := 1
		if strings.Contains(ej.Name, deploymentToken) {
			n = len(deployments)
		}
		asserted += n * len(ej.Steps)
	}
	fmt.Printf("%s ran and did its job: %d asserted check(s) executed and passed across %d deployment(s)\n",
		p.Workflow, asserted, len(deployments))
	return nil
}
