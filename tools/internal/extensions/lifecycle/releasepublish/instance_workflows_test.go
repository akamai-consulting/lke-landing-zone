package releasepublish

// instance_workflows_test.go — the verdict and the dispatch/wait loop of
// `llz ci assert-instance-workflow`, without a forge.
//
// The verdict tests pin the outcomes a release run cannot be trusted to produce
// on demand: the startup_failure that hid secret-rotation.yml for months, a
// green run whose checks were all skipped, a missing matrix leg, and a dry-run
// dispatch that executed a mutating job.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func mustProfile(t *testing.T, name string) workflowProfile {
	t.Helper()
	p, ok := lookupProfile(name)
	if !ok {
		t.Fatalf("no profile %q", name)
	}
	return p
}

// greenRun builds the run a healthy dispatch of p produces for deployments:
// every expected job present, successful, with every asserted step run — named
// the way GitHub names jobs inside a called workflow.
func greenRun(p workflowProfile, deployments []string) runView {
	v := runView{Status: "completed", Conclusion: "success", URL: "https://example.test/run/1"}
	for _, ej := range p.Jobs {
		for _, name := range expandAll(ej.Name, deployments) {
			j := runJob{Name: "call / " + name, Status: "completed", Conclusion: "success", URL: "https://example.test/job"}
			if ej.Source == discoverSource {
				j.Name = "call / discover / " + name
			}
			j.Steps = append(j.Steps, runStep{Name: "Set up job", Status: "completed", Conclusion: "success"})
			for _, s := range ej.Steps {
				j.Steps = append(j.Steps, runStep{Name: s, Status: "completed", Conclusion: "success"})
			}
			v.Jobs = append(v.Jobs, j)
		}
	}
	// The mutating jobs a dry run skips are still LISTED, unexpanded, as skipped.
	for _, m := range p.MustNotRun {
		v.Jobs = append(v.Jobs, runJob{Name: "call / " + strings.ReplaceAll(m, deploymentToken, "${{ matrix.region }}"),
			Status: "completed", Conclusion: "skipped"})
	}
	return v
}

func TestEveryProfileJudgesItsOwnGreenRunAsPassing(t *testing.T) {
	for _, p := range instanceWorkflowProfiles {
		if probs := evaluateWorkflowRun(p, []string{"e2e"}, greenRun(p, []string{"e2e"})); len(probs) != 0 {
			t.Errorf("profile %s: a fully green run was judged failing: %v", p.Name, probs)
		}
	}
}

// The secret-rotation incident, pinned: no jobs, no logs, and a diagnosis that
// names the caller-permissions rule first.
func TestStartupFailureIsNamedAsSuch(t *testing.T) {
	p := mustProfile(t, "rotation-linode-pat")
	probs := evaluateWorkflowRun(p, []string{"e2e"}, runView{Status: "completed", Conclusion: "startup_failure", URL: "u"})
	if len(probs) != 1 || !strings.Contains(probs[0], "startup_failure") || !strings.Contains(probs[0], "permissions") {
		t.Fatalf("startup_failure must be one specific diagnosis naming the permissions rule, got %v", probs)
	}
}

func TestARunWithNoJobsIsNotAPass(t *testing.T) {
	p := mustProfile(t, "scheduled-checks")
	probs := evaluateWorkflowRun(p, []string{"e2e"}, runView{Status: "completed", Conclusion: "success"})
	if len(probs) != 1 || !strings.Contains(probs[0], "no jobs") {
		t.Fatalf("a run with zero jobs passed or was misdiagnosed: %v", probs)
	}
}

// THE VACUOUS GREEN this verb exists for: cluster-access says no cluster, every
// check skips, every job succeeds.
func TestSkippedChecksFailAGreenRun(t *testing.T) {
	p := mustProfile(t, "scheduled-checks")
	v := greenRun(p, []string{"e2e"})
	for i := range v.Jobs {
		for k := range v.Jobs[i].Steps {
			if strings.HasPrefix(v.Jobs[i].Steps[k].Name, "Check ") {
				v.Jobs[i].Steps[k].Conclusion = "skipped"
			}
		}
	}
	probs := evaluateWorkflowRun(p, []string{"e2e"}, v)
	if len(probs) == 0 {
		t.Fatal("a run whose checks were all skipped passed — the gate examined nothing and said green")
	}
	for _, pr := range probs {
		if !strings.Contains(pr, "WITHOUT RUNNING") || !strings.Contains(pr, "Cluster access") {
			t.Errorf("a skipped check should say it did not run and where to look, got: %s", pr)
		}
	}
}

func TestMissingMatrixLegFails(t *testing.T) {
	p := mustProfile(t, "scheduled-checks")
	// The run only covered e2e, but the caller says e2e AND e2e-b have clusters.
	probs := evaluateWorkflowRun(p, []string{"e2e", "e2e-b"}, greenRun(p, []string{"e2e"}))
	var legs int
	for _, pr := range probs {
		if strings.Contains(pr, "(e2e-b)") && strings.Contains(pr, "never appeared") {
			legs++
		}
	}
	if legs != 5 {
		t.Fatalf("want one 'never appeared' per per-deployment job for the missing leg (5), got %d: %v", legs, probs)
	}
}

func TestMissingStepIsReportedWithWhatIsPresent(t *testing.T) {
	p := mustProfile(t, "rotation-linode-pat-revoke")
	v := greenRun(p, []string{"e2e"})
	for i := range v.Jobs {
		if strings.HasSuffix(v.Jobs[i].Name, "Revoke old LINODE_API_TOKEN PATs") {
			v.Jobs[i].Steps = v.Jobs[i].Steps[:1] // only "Set up job"
		}
	}
	probs := evaluateWorkflowRun(p, []string{"e2e"}, v)
	if len(probs) != 1 || !strings.Contains(probs[0], `"Set up job"`) {
		t.Fatalf("a missing step should name the steps that ARE present, got %v", probs)
	}
}

func TestFailedExpectedJobAndFailedStep(t *testing.T) {
	p := mustProfile(t, "scheduled-checks")
	v := greenRun(p, []string{"e2e"})
	for i := range v.Jobs {
		switch {
		case strings.HasSuffix(v.Jobs[i].Name, "Template drift"):
			v.Jobs[i].Conclusion = "failure"
		case strings.HasSuffix(v.Jobs[i].Name, "PrometheusRule Health (e2e)"):
			v.Jobs[i].Steps[1].Conclusion = "failure" // job green, step red: continue-on-error shape
		}
	}
	probs := strings.Join(evaluateWorkflowRun(p, []string{"e2e"}, v), "\n")
	if !strings.Contains(probs, `"call / Template drift" concluded "failure"`) {
		t.Errorf("failed expected job not reported: %s", probs)
	}
	if !strings.Contains(probs, `step "Check PrometheusRule CRs present" concluded "failure"`) {
		t.Errorf("a failed step inside a green job not reported: %s", probs)
	}
}

// A dry-run dispatch that ran a mutating job must fail, whatever else is green.
func TestMutatingJobOnADryRunFails(t *testing.T) {
	p := mustProfile(t, "rotation-linode-pat")
	v := greenRun(p, []string{"e2e"})
	v.Jobs = append(v.Jobs, runJob{Name: "call / Rotate the in-cluster PAT on e2e", Status: "completed", Conclusion: "success"})
	probs := evaluateWorkflowRun(p, []string{"e2e"}, v)
	if len(probs) != 1 || !strings.Contains(probs[0], "EXECUTED") {
		t.Fatalf("a mutating job that ran on a dry-run dispatch must fail the verb, got %v", probs)
	}
	// And a skipped one (the normal case) must not.
	v.Jobs[len(v.Jobs)-1].Conclusion = "skipped"
	if probs := evaluateWorkflowRun(p, []string{"e2e"}, v); len(probs) != 0 {
		t.Fatalf("a SKIPPED mutating job is the dry run working, not a failure: %v", probs)
	}
}

func TestUnassertedFailedJobFailsButUnassertedSkipDoesNot(t *testing.T) {
	p := mustProfile(t, "rotation-tf-state-key")
	v := greenRun(p, []string{"e2e"})
	v.Jobs = append(v.Jobs, runJob{Name: "call / Something new", Conclusion: "skipped"})
	if probs := evaluateWorkflowRun(p, []string{"e2e"}, v); len(probs) != 0 {
		t.Fatalf("an unasserted SKIPPED job is a branch not taken, not a failure: %v", probs)
	}
	v.Jobs[len(v.Jobs)-1].Conclusion = "failure"
	if probs := evaluateWorkflowRun(p, []string{"e2e"}, v); len(probs) != 1 || !strings.Contains(probs[0], "Something new") {
		t.Fatalf("an unasserted job that FAILED is a delivered job failing: %v", probs)
	}
}

func TestRunConclusionIsACatchAll(t *testing.T) {
	p := mustProfile(t, "rotation-tf-state-key-revoke")
	v := greenRun(p, []string{"e2e"})
	v.Conclusion = "failure"
	if probs := evaluateWorkflowRun(p, []string{"e2e"}, v); len(probs) != 1 {
		t.Fatalf("a run GitHub marks failed must not pass because no rule named why: %v", probs)
	}
}

// "Setup" must not match "Setup kubectl", and a nested name matches on its tail.
func TestJobMatchingIsBySuffixSegmentNotSubstring(t *testing.T) {
	cases := []struct {
		observed, want string
		match          bool
	}{
		{"Setup", "Setup", true},
		{"call / Setup", "Setup", true},
		{"call / discover / Discover deployments", "Discover deployments", true},
		{"call / Setup kubectl", "Setup", false},
		{"call / Pre-Setup", "Setup", false},
	}
	for _, c := range cases {
		if got := jobMatches(c.observed, c.want); got != c.match {
			t.Errorf("jobMatches(%q, %q) = %v, want %v", c.observed, c.want, got, c.match)
		}
	}
}

// ── The flow, with a fake gh ─────────────────────────────────────────────────

type fakeWorkflowForge struct {
	listBefore, listAfter string // `gh run list` payloads before / after the dispatch
	dispatchOut           string
	dispatchErrs          int // the first N dispatches fail
	views                 []runView
	viewErr               error
	calls                 [][]string
	hosts                 []string
	dispatched            bool
	viewPolls             int
	now                   time.Time
}

func (f *fakeWorkflowForge) install(t *testing.T) {
	origGH, origSleep, origNow := wfGH, wfSleep, wfNow
	t.Cleanup(func() { wfGH, wfSleep, wfNow = origGH, origSleep, origNow })
	f.now = time.Unix(1_700_000_000, 0)
	wfNow = func() time.Time { return f.now }
	wfSleep = func(d time.Duration) { f.now = f.now.Add(d) }
	wfGH = func(_, host string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, args)
		f.hosts = append(f.hosts, host)
		switch {
		case args[0] == "run" && args[1] == "list":
			if f.dispatched {
				return []byte(f.listAfter), nil
			}
			return []byte(f.listBefore), nil
		case args[0] == "workflow" && args[1] == "run":
			if f.dispatchErrs > 0 {
				f.dispatchErrs--
				return nil, errors.New("HTTP 422: workflow not found on ref")
			}
			f.dispatched = true
			return []byte(f.dispatchOut), nil
		case args[0] == "run" && args[1] == "view":
			if f.viewErr != nil {
				return nil, f.viewErr
			}
			i := f.viewPolls
			if i >= len(f.views) {
				i = len(f.views) - 1
			}
			f.viewPolls++
			b, _ := json.Marshal(f.views[i])
			return b, nil
		case args[0] == "run" && args[1] == "cancel":
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected gh call %v", args)
	}
}

func (f *fakeWorkflowForge) called(verb ...string) bool {
	for _, c := range f.calls {
		if len(c) >= len(verb) && strings.Join(c[:len(verb)], " ") == strings.Join(verb, " ") {
			return true
		}
	}
	return false
}

func wfOpts(profile string) InstanceWorkflowOpts {
	return InstanceWorkflowOpts{Instance: "o/inst", Token: "t", Profile: profile, Deployments: []string{"e2e"},
		Interval: time.Second, Host: "ghes.example.test"}
}

func TestFlowLocatesTheRunByEliminationAndPasses(t *testing.T) {
	p := mustProfile(t, "rotation-linode-pat")
	f := &fakeWorkflowForge{
		listBefore:   `[{"databaseId":10},{"databaseId":9}]`,
		listAfter:    `[{"databaseId":11},{"databaseId":10},{"databaseId":9}]`,
		dispatchErrs: 1,
		views:        []runView{{Status: "in_progress"}, greenRun(p, []string{"e2e"})},
	}
	f.install(t)
	out := captureStdout(t, func() {
		if err := RunAssertInstanceWorkflow(wfOpts(p.Name)); err != nil {
			t.Errorf("green run failed: %v", err)
		}
	})
	if !strings.Contains(out, "Watching run 11") {
		t.Errorf("the new run (11) was not the one watched:\n%s", out)
	}
	if !strings.Contains(out, "did its job") {
		t.Errorf("no success line:\n%s", out)
	}
	for _, h := range f.hosts {
		if h != "ghes.example.test" {
			t.Fatalf("a gh call went to %q, not --host", h)
		}
	}
	// The argv carries every apply input as an explicit false.
	var dispatch []string
	for _, c := range f.calls {
		if c[0] == "workflow" {
			dispatch = c
		}
	}
	argv := strings.Join(dispatch, " ")
	for _, want := range []string{"secret-rotation.yml", "--ref main", "-f scope=linode-pat", "-f confirm=rotate:linode-pat", "-f pat-apply=false"} {
		if !strings.Contains(argv, want) {
			t.Errorf("dispatch argv %q lacks %q", argv, want)
		}
	}
}

func TestFlowUsesTheRunURLWhenGhPrintsIt(t *testing.T) {
	p := mustProfile(t, "scheduled-checks")
	f := &fakeWorkflowForge{
		listBefore:  `[]`,
		listAfter:   `[]`, // would never resolve by elimination
		dispatchOut: "https://github.com/o/inst/actions/runs/4242\n",
		views:       []runView{greenRun(p, []string{"e2e"})},
	}
	f.install(t)
	out := captureStdout(t, func() {
		if err := RunAssertInstanceWorkflow(wfOpts(p.Name)); err != nil {
			t.Errorf("unexpected: %v", err)
		}
	})
	if !strings.Contains(out, "Watching run 4242") {
		t.Errorf("did not use the run id gh printed:\n%s", out)
	}
}

func TestFlowFailsWhenNoRunAppears(t *testing.T) {
	f := &fakeWorkflowForge{listBefore: `[{"databaseId":1}]`, listAfter: `[{"databaseId":1}]`}
	f.install(t)
	var err error
	captureStdout(t, func() { err = RunAssertInstanceWorkflow(wfOpts("scheduled-checks")) })
	if err == nil || !strings.Contains(err.Error(), "no new workflow_dispatch run appeared") {
		t.Fatalf("want a not-located error, got %v", err)
	}
}

func TestFlowFailsWhenDispatchNeverSucceeds(t *testing.T) {
	f := &fakeWorkflowForge{listBefore: `[]`, dispatchErrs: 99}
	f.install(t)
	var err error
	captureStdout(t, func() { err = RunAssertInstanceWorkflow(wfOpts("scheduled-checks")) })
	if err == nil || !strings.Contains(err.Error(), "was not dispatched") {
		t.Fatalf("want a dispatch error, got %v", err)
	}
}

func TestFlowTimesOutAndCancels(t *testing.T) {
	f := &fakeWorkflowForge{
		listBefore: `[]`, listAfter: `[{"databaseId":5}]`,
		views: []runView{{Status: "in_progress"}},
	}
	f.install(t)
	o := wfOpts("scheduled-checks")
	o.Timeout = time.Minute
	var err error
	captureStdout(t, func() { err = RunAssertInstanceWorkflow(o) })
	if err == nil || !strings.Contains(err.Error(), "did not complete") {
		t.Fatalf("want a timeout, got %v", err)
	}
	if !f.called("run", "cancel") {
		t.Error("a timed-out run was left running — it would race the teardown")
	}
}

func TestFlowReportsUnreadableRunOnTimeout(t *testing.T) {
	f := &fakeWorkflowForge{
		listBefore: `[]`, listAfter: `[{"databaseId":5}]`,
		viewErr: errors.New("HTTP 502"),
	}
	f.install(t)
	o := wfOpts("scheduled-checks")
	o.Timeout = 30 * time.Second
	var err error
	captureStdout(t, func() { err = RunAssertInstanceWorkflow(o) })
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("an unreadable run must say why it could not tell, got %v", err)
	}
}

func TestFlowSurfacesTheVerdict(t *testing.T) {
	p := mustProfile(t, "scheduled-checks")
	f := &fakeWorkflowForge{
		listBefore: `[]`, listAfter: `[{"databaseId":7}]`,
		views: []runView{{Status: "completed", Conclusion: "startup_failure", URL: "u"}},
	}
	f.install(t)
	var err error
	out := captureStdout(t, func() { err = RunAssertInstanceWorkflow(wfOpts(p.Name)) })
	if err == nil || !strings.Contains(out, "::error title=scheduled-checks.yml::") {
		t.Fatalf("want an annotated failure, got err=%v out=\n%s", err, out)
	}
}

func TestOptionValidation(t *testing.T) {
	cases := []struct {
		mut  func(*InstanceWorkflowOpts)
		want string
	}{
		{func(o *InstanceWorkflowOpts) { o.Instance = "" }, "--instance"},
		{func(o *InstanceWorkflowOpts) { o.Token = "" }, "GH_TOKEN"},
		{func(o *InstanceWorkflowOpts) { o.Profile = "nope" }, "unknown --profile"},
		// The vacuity guard: no deployments would expand to no expected legs.
		{func(o *InstanceWorkflowOpts) { o.Deployments = []string{" "} }, "--deployment is required"},
	}
	for _, c := range cases {
		o := wfOpts("scheduled-checks")
		c.mut(&o)
		err := RunAssertInstanceWorkflow(o)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want error containing %q, got %v", c.want, err)
		}
	}
}

func TestCobraRejectsBadNumbers(t *testing.T) {
	for _, args := range [][]string{{"--interval", "0"}, {"--timeout", "-1"}} {
		c := AssertInstanceWorkflowCmd()
		c.SetArgs(append([]string{"--profile", "scheduled-checks", "--instance", "o/r", "--deployment", "e2e"}, args...))
		c.SetOut(new(strings.Builder))
		c.SetErr(new(strings.Builder))
		if err := c.Execute(); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestExportedProfilesMirrorTheTable(t *testing.T) {
	got := InstanceWorkflowProfiles()
	if len(got) != len(instanceWorkflowProfiles) || len(got) < 2 {
		t.Fatalf("exported %d profiles, table has %d", len(got), len(instanceWorkflowProfiles))
	}
	for i, p := range got {
		if p.Name != instanceWorkflowProfiles[i].Name || p.Workflow != instanceWorkflowProfiles[i].Workflow {
			t.Errorf("exported profile %d = %+v, table has %s/%s", i, p, instanceWorkflowProfiles[i].Name, instanceWorkflowProfiles[i].Workflow)
		}
	}
}

func TestRunFlagJudgesWithoutDispatching(t *testing.T) {
	p := mustProfile(t, "scheduled-checks")
	f := &fakeWorkflowForge{views: []runView{greenRun(p, []string{"e2e"})}}
	f.install(t)
	o := wfOpts(p.Name)
	o.RunID = 99
	out := captureStdout(t, func() {
		if err := RunAssertInstanceWorkflow(o); err != nil {
			t.Errorf("unexpected: %v", err)
		}
	})
	if f.called("workflow", "run") || f.called("run", "list") {
		t.Fatal("--run must judge the named run and dispatch nothing")
	}
	if !strings.Contains(out, "Judging existing run 99") {
		t.Errorf("did not say it was judging an existing run:\n%s", out)
	}
}
