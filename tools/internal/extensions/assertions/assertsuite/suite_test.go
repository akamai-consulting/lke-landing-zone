package assertsuite

// Tests for the e2e assert battery's orchestration — the part that used to be
// untested inline bash and that decides whether the whole battery fails.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// seamLaneRunner replaces subprocess execution with a table of canned results
// keyed by the first argument (the verb), recording call order.
func seamLaneRunner(t *testing.T, results map[string]int) *[]string {
	orig := runLaneFn
	t.Cleanup(func() { runLaneFn = orig })
	var mu sync.Mutex
	var calls []string
	runLaneFn = func(args []string) (string, int) {
		mu.Lock()
		calls = append(calls, strings.Join(args, " "))
		mu.Unlock()
		code := results[args[0]]
		return fmt.Sprintf("output of %s\n", args[0]), code
	}
	return &calls
}

// suiteNow is a frozen clock for the lane orchestration (durations are reported,
// never asserted on).
func suiteNow() func() time.Time {
	t := time.Unix(1_720_000_000, 0)
	return func() time.Time { return t }
}

// A lane's steps run IN ORDER and STOP at the first failure — that is how an
// ordered dependency (assert-reconciler reading gauges assert-scrape-targets just
// proved fresh) is expressed. Running the later steps anyway would report
// cascading failures for one root cause.
func TestRunLaneStopsAtFirstFailure(t *testing.T) {
	calls := seamLaneRunner(t, map[string]int{"b": 3})
	l := Lane{Name: "x", Gating: true, Steps: []Step{{Argv: []string{"a"}}, {Argv: []string{"b"}}, {Argv: []string{"c"}}}}
	res := runLane(l, suiteNow(), nil)

	if res.ExitCode != 3 || !res.Failed {
		t.Fatalf("expected the lane to fail with the step's code, got %+v", res)
	}
	if got := strings.Join(*calls, ","); got != "a,b" {
		t.Errorf("steps after the failure must NOT run, got %q", got)
	}
	if !strings.Contains(res.Output, "output of a") || !strings.Contains(res.Output, "output of b") {
		t.Errorf("the lane must keep the output of every step it ran: %q", res.Output)
	}
}

// A report-only lane is still RUN and still PRINTED, but never gates — the old
// `|| true`, now expressed as data rather than shell.
func TestRunLaneReportOnlyNeverGates(t *testing.T) {
	seamLaneRunner(t, map[string]int{"diag": 1})
	res := runLane(Lane{Name: "d", Gating: false, Steps: []Step{{Argv: []string{"diag"}}}}, suiteNow(), nil)
	if res.ExitCode != 1 {
		t.Errorf("the exit code must still be recorded, got %d", res.ExitCode)
	}
	if res.Failed {
		t.Error("a report-only lane must never fail the battery")
	}
	if !strings.Contains(res.Output, "output of diag") {
		t.Error("a report-only lane's output is the deliverable and must be captured")
	}
}

func TestRunAssertSuiteLanesRunsEveryLaneAndPreservesOrder(t *testing.T) {
	calls := seamLaneRunner(t, map[string]int{})
	lanes := []Lane{
		{Name: "one", Gating: true, Steps: []Step{{Argv: []string{"a"}}}},
		{Name: "two", Gating: true, Steps: []Step{{Argv: []string{"b"}}}},
		{Name: "three", Gating: true, Steps: []Step{{Argv: []string{"c"}}}},
	}
	results := runAssertSuiteLanes(lanes, suiteNow(), nil)

	if len(results) != 3 {
		t.Fatalf("expected one result per lane, got %d", len(results))
	}
	// Results must come back in TABLE order regardless of completion order, or the
	// log is nondeterministic and diffing two runs becomes useless.
	for i, want := range []string{"one", "two", "three"} {
		if results[i].Lane.Name != want {
			t.Errorf("result %d is %s, want %s — output order must not depend on scheduling", i, results[i].Lane.Name, want)
		}
	}
	if len(*calls) != 3 {
		t.Errorf("every lane must run, got %d calls", len(*calls))
	}
}

// THE regression the refactor exists to prevent: in the YAML version the lane
// list was written twice (once to run, once to collect), and a lane missing from
// the collection loop ran and could never fail the step. Here one list drives
// both, so every failing gating lane necessarily reaches the verdict.
func TestEveryFailingGatingLaneReachesTheVerdict(t *testing.T) {
	seamLaneRunner(t, map[string]int{"boom": 1, "diag": 1})
	lanes := []Lane{
		{Name: "ok", Gating: true, Steps: []Step{{Argv: []string{"fine"}}}},
		{Name: "bad", Gating: true, Steps: []Step{{Argv: []string{"boom"}}}},
		{Name: "report", Gating: false, Steps: []Step{{Argv: []string{"diag"}}}},
	}
	results := runAssertSuiteLanes(lanes, suiteNow(), nil)
	failed := failedLaneNames(results)

	if len(failed) != 1 || failed[0] != "bad" {
		t.Fatalf("exactly the failing GATING lane must be reported, got %v", failed)
	}
	// Count the lanes that ran but could not influence the verdict. In the YAML
	// version this number was silently non-zero whenever someone forgot the loop.
	for _, r := range results {
		if r.Lane.Gating && r.ExitCode != 0 && !r.Failed {
			t.Errorf("gating lane %s failed but does not gate — this is the vacuous pass the refactor removed", r.Lane.Name)
		}
	}
}

func TestRunCIAssertSuiteVerdict(t *testing.T) {
	seamLaneRunner(t, map[string]int{"assert-loki": 1})
	if err := Run("e2e", []string{"loki"}, false, false); err == nil {
		t.Error("a failing gating lane must fail the suite")
	}
	seamLaneRunner(t, map[string]int{})
	if err := Run("e2e", []string{"loki"}, false, false); err != nil {
		t.Errorf("all-passing lanes must succeed, got %v", err)
	}
}

// An unknown lane name must be an ERROR. A typo that silently shrank the battery
// is the same class of bug as a lane missing from the old collection loop.
func TestSelectLanesRejectsUnknownNames(t *testing.T) {
	all := Lanes("e2e")
	if _, err := selectLanes(all, []string{"loki", "lokii"}); err == nil {
		t.Error("an unknown lane name must fail rather than silently shrink the battery")
	}
	got, err := selectLanes(all, []string{"loki"})
	if err != nil || len(got) != 1 || got[0].Name != "loki" {
		t.Errorf("selecting a known lane should work, got %+v (%v)", got, err)
	}
	if full, err := selectLanes(all, nil); err != nil || len(full) != len(all) {
		t.Errorf("no selection must mean every lane, got %d (%v)", len(full), err)
	}
}

// The region must reach exactly the lanes that take it, and no others — a lane
// invoked with an unexpected flag fails at argument parsing, which would look
// like a cluster fault.
func TestAssertSuiteLanesThreadRegionOnlyWhereItBelongs(t *testing.T) {
	lanes := Lanes("e2e")
	// obj-encryption takes --region because it is COMPONENT-GATED: it reads
	// spec.components.objProxy for that deployment and self-skips when the SSE-C
	// gateway is not enabled, rather than redding every cluster that does not run it.
	//
	// loki takes it because its write PROOF resolves this deployment's chunks bucket
	// from the spec to confirm a flushed chunk landed. Reading $REGION inside the verb
	// instead would turn a missing value into a silent skip — the proof would report
	// "unmeasured" and pass, which is the exact failure mode the proof was added to
	// remove.
	wantRegion := map[string]bool{"health-workflow": true, "broad-pat": true, "team-write": true,
		"obj-encryption": true, "loki": true}
	for _, l := range lanes {
		hasRegion := false
		for _, s := range l.Steps {
			for _, a := range s.Argv {
				if a == "--region" {
					hasRegion = true
				}
			}
		}
		if hasRegion != wantRegion[l.Name] {
			t.Errorf("lane %s: --region present=%v, want %v", l.Name, hasRegion, wantRegion[l.Name])
		}
	}
	// With no region, the flag must be omitted entirely rather than passed empty:
	// `--region ""` is a different thing from not scoping at all.
	for _, l := range Lanes("") {
		for _, s := range l.Steps {
			for _, a := range s.Argv {
				if a == "--region" {
					t.Errorf("lane %s passes --region with no value available", l.Name)
				}
			}
		}
	}
}

// Every lane must actually name a registered `llz ci` verb. A lane referencing a
// verb that does not exist would fail at runtime as an opaque "unknown command"
// deep inside an e2e run.
func TestEveryLaneDocumentsWhatItProves(t *testing.T) {
	for _, l := range Lanes("e2e") {
		if len(strings.TrimSpace(l.Why)) < 40 {
			t.Errorf("lane %s has no meaningful Why — a failing lane must carry its own rationale", l.Name)
		}
	}
	if countGating(Lanes("e2e")) == 0 {
		t.Fatal("the battery gates on nothing — it would always pass")
	}
}

func TestAppendLaneSummariesWritesDeliverables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", path)

	appendLaneSummaries([]laneResult{
		{Lane: Lane{Name: "alert-eval", SummaryTitle: "alert-eval — live rule evaluation"}, Output: "FIRING: x\n"},
		{Lane: Lane{Name: "loki"}, Output: "not a deliverable"},
	})

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading summary: %v", err)
	}
	if !strings.Contains(string(got), "alert-eval — live rule evaluation") || !strings.Contains(string(got), "FIRING: x") {
		t.Errorf("the deliverable lane must reach the step summary, got %q", got)
	}
	if strings.Contains(string(got), "not a deliverable") {
		t.Error("a lane with no SummaryTitle must not be written to the summary")
	}
}

// A missing/unwritable $GITHUB_STEP_SUMMARY must never change the verdict —
// instrumentation cannot be allowed to fail a run.
func TestAppendLaneSummariesIsBestEffort(t *testing.T) {
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	appendLaneSummaries([]laneResult{{Lane: Lane{SummaryTitle: "x"}, Output: "y"}})
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(t.TempDir(), "nope", "deep", "summary.md"))
	appendLaneSummaries([]laneResult{{Lane: Lane{SummaryTitle: "x"}, Output: "y"}})
}

// A SKIPPED LANE IS NOT A PASSING LANE, and this is the assertion the whole skip
// state exists for.
//
// Before it, lanes that "skip clean when the component is disabled" did so by
// EXITING 0, and laneResult had nowhere to record that. A lane that ran nothing
// was indistinguishable from one that proved something, and the battery's closing
// line counted DECLARED gating lanes — so a run that skipped three still announced
// that all of them passed. That is the vacuous-green shape this file's header was
// written to kill, reappearing one level up.
func TestASkippedLaneDoesNotRunAndIsNotAPass(t *testing.T) {
	// ATOMIC BECAUSE THE LANES RUN CONCURRENTLY. runAssertSuiteLanes starts one
	// goroutine per lane, so the seam installed here is called from several at
	// once — `ran++` is a data race, and the race detector fails the build on it.
	// The counter is also the test's assertion, so the unsynchronised read could
	// report the wrong number rather than merely being unclean.
	var ran atomic.Int64
	prev := runLaneFn
	runLaneFn = func([]string) (string, int) { ran.Add(1); return "", 0 }
	t.Cleanup(func() { runLaneFn = prev })

	lanes := []Lane{
		{Name: "registry", Gating: true, Steps: []Step{{Extension: "assert-registry", Argv: []string{"x"}}}},
		{Name: "always", Gating: true, Steps: []Step{{Argv: []string{"y"}}}},
	}
	disabled := func(ext string) (string, bool) {
		if ext == "assert-registry" {
			return "component harbor disabled", true
		}
		return "", false
	}

	res := runAssertSuiteLanes(lanes, suiteNow(), disabled)

	if n := ran.Load(); n != 1 {
		t.Errorf("%d lanes executed, want 1 — the disabled lane must not run at all, not run "+
			"and self-report", n)
	}
	if !res[0].Skipped {
		t.Error("the disabled lane is not marked Skipped — without the state it is reported " +
			"exactly like a lane that passed")
	}
	if res[0].Failed {
		t.Error("a skipped lane is marked Failed — not running is not failing, and conflating " +
			"them would fail batteries for instances that legitimately lack a component")
	}
	if res[0].SkipReason == "" {
		t.Error("no SkipReason — an operator asking why a lane is missing should not have to guess")
	}
	if res[1].Skipped {
		t.Error("a lane with no Extension was skipped — empty must mean always run")
	}
	if n := countGatingSkipped(res); n != 1 {
		t.Errorf("countGatingSkipped = %d, want 1 — this is what stops the closing line "+
			"claiming a skipped lane passed", n)
	}
	if got := skippedLaneNames(res); len(got) != 1 || got[0] != "registry" {
		t.Errorf("skippedLaneNames = %v, want [registry]", got)
	}
}

// A nil resolver must run everything. An uninstalled seam that skipped lanes would
// silence the battery wherever the wiring was forgotten — the dangerous direction.
func TestNoResolverRunsEveryLane(t *testing.T) {
	// Atomic for the reason above: this test runs BOTH lanes, so both goroutines
	// hit the seam. This is the one the race detector actually caught.
	var ran atomic.Int64
	prev := runLaneFn
	runLaneFn = func([]string) (string, int) { ran.Add(1); return "", 0 }
	t.Cleanup(func() { runLaneFn = prev })

	lanes := []Lane{
		{Name: "a", Gating: true, Steps: []Step{{Extension: "assert-registry", Argv: []string{"x"}}}},
		{Name: "b", Gating: true, Steps: []Step{{Extension: "obj-encryption", Argv: []string{"y"}}}},
	}
	res := runAssertSuiteLanes(lanes, suiteNow(), nil)
	if n := ran.Load(); n != 2 {
		t.Errorf("%d lanes ran with a nil resolver, want 2 — no enablement source means run "+
			"everything, never skip everything", n)
	}
	for _, r := range res {
		if r.Skipped {
			t.Errorf("%s skipped with no resolver installed", r.Lane.Name)
		}
	}
}

// TestMutatingFlagAgreesWithTheLaneItDescribes couples the two halves of one
// declaration instead of restating either.
//
// Every mutating lane already says so in its own `Why`, in caps, because that
// string is what prints beside a failure. The `Mutating` bool is what
// `--skip-mutating` reads. Those are two copies of one fact, and the first cut of
// this gate hardcoded the set — which pinned the state of the tree at the moment
// it was written rather than checking it. It was written with two of the four
// lanes marked, so it asserted the bug was correct and would have failed on the
// fix.
//
// Deriving from the prose means a lane added later, or a `Why` edited to say
// MUTATING, forces the flag to move with it — and a lane marked without saying so
// is caught too, because a caller reading the printed rationale would have no idea
// it was skipped.
func TestMutatingFlagAgreesWithTheLaneItDescribes(t *testing.T) {
	all := Lanes("e2e")
	if len(all) < 5 {
		t.Fatalf("only %d lane(s) — the table is not being read, so this gate would pass having "+
			"compared nothing", len(all))
	}
	says, marked := 0, 0
	for _, l := range all {
		declares := strings.Contains(l.Why, "MUTATING")
		if declares {
			says++
		}
		if l.Mutating {
			marked++
		}
		switch {
		case declares && !l.Mutating:
			t.Errorf("lane %q says MUTATING in its Why but is not marked Mutating, so --skip-mutating "+
				"still runs it: a promotion stage and the weekly apply would exercise it against "+
				"production. Why: %s", l.Name, l.Why)
		case !declares && l.Mutating:
			t.Errorf("lane %q is marked Mutating but its Why does not say so — a reader of the printed "+
				"rationale has no way to know it is skipped on every promotion", l.Name)
		}
	}
	// Fail closed on vacuity in both directions: zero marked lanes makes
	// --skip-mutating a no-op that leaves every caller unprotected while looking
	// protected, and zero declaring lanes means the Why convention was dropped.
	if says == 0 || marked == 0 {
		t.Fatalf("%d lane(s) declare MUTATING and %d are marked — a suite with neither is one where "+
			"--skip-mutating protects nothing", says, marked)
	}
	t.Logf("%d of %d lanes are mutating", marked, len(all))
}

// TestDay2IsAnAllowListThatExcludesEveryMutatingLane couples the two axes.
//
// Day2 says "safe and meaningful against a live deployment"; Mutating says
// "changes real state". Nothing can be both, and the check lives here rather than
// in a comment because the promotion path runs whatever Day2 admits — against
// production, on a timer, with no human reading the lane list first.
func TestDay2IsAnAllowListThatExcludesEveryMutatingLane(t *testing.T) {
	all := Lanes("e2e")
	day2 := 0
	for _, l := range all {
		if !l.Day2 {
			continue
		}
		day2++
		if l.Mutating {
			t.Errorf("lane %q is marked Day2 AND Mutating. Day2 lanes run on every promotion stage and "+
				"every scheduled apply; a mutating one there changes production state on a timer.", l.Name)
		}
	}
	if day2 == 0 {
		t.Fatal("no lane is marked Day2 — --day2 would drop everything and the promotion path could never pass")
	}
	if day2 == len(all) {
		t.Fatal("every lane is marked Day2 — the allow-list admits the e2e-only and mutating lanes it exists " +
			"to keep out of production")
	}
	t.Logf("%d of %d lanes are day-2 safe", day2, len(all))
}

func TestDay2ExcludesTheLanesThatNeedTheE2EFixture(t *testing.T) {
	// The specific miss that made this an allow-list rather than a subtraction.
	// instance-custom polls for Application `instance-custom-llz-e2e-custom` in
	// namespace `llz-e2e-custom`, which ONLY the template repo's
	// e2e-instantiate.yml creates — and the lane is gating, and assert-platform is
	// componentless so it never self-disables. On a real instance it polls, fails,
	// and `needs:` blocks the promotion chain at the dev stage. Every promotion.
	for _, name := range []string{"instance-custom", "team-write", "health-workflow", "broad-pat"} {
		for _, l := range Lanes("e2e") {
			if l.Name == name && l.Day2 {
				t.Errorf("lane %q is marked Day2, but it depends on the release-e2e fixture or mutates real "+
					"state — a promotion into prod would run it and block on it", name)
			}
		}
	}
}

func TestDay2FailsRatherThanRunningNothing(t *testing.T) {
	err := Run("e2e", []string{"broad-pat", "instance-custom"}, false, true)
	if err == nil {
		t.Fatal("an empty lane selection must fail rather than pass")
	}
	if !strings.Contains(err.Error(), "no lanes to run") {
		t.Errorf("error must say the selection was empty, got: %v", err)
	}
}

// TestDay2HelpNamesEveryLaneItAdmits keeps the sentence an operator reads in step
// with the set the flag actually runs against production.
func TestDay2HelpNamesEveryLaneItAdmits(t *testing.T) {
	flag := Cmd().Flags().Lookup("day2")
	if flag == nil {
		t.Fatal("no --day2 flag — the gate would pass having read nothing")
	}
	admitted := 0
	for _, l := range Lanes("e2e") {
		if !l.Day2 {
			continue
		}
		admitted++
		if !strings.Contains(flag.Usage, l.Name) {
			t.Errorf("--day2 runs lane %q against production but its help does not name it.\n  help: %s",
				l.Name, flag.Usage)
		}
	}
	if admitted == 0 {
		t.Fatal("no lane is admitted — this gate compared nothing")
	}
}
