package main

// Tests for the e2e assert battery's orchestration — the part that used to be
// untested inline bash and that decides whether the whole battery fails.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	l := suiteLane{Name: "x", Gating: true, Steps: [][]string{{"a"}, {"b"}, {"c"}}}
	res := runLane(l, suiteNow())

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
	res := runLane(suiteLane{Name: "d", Gating: false, Steps: [][]string{{"diag"}}}, suiteNow())
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
	lanes := []suiteLane{
		{Name: "one", Gating: true, Steps: [][]string{{"a"}}},
		{Name: "two", Gating: true, Steps: [][]string{{"b"}}},
		{Name: "three", Gating: true, Steps: [][]string{{"c"}}},
	}
	results := runAssertSuiteLanes(lanes, suiteNow())

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
	lanes := []suiteLane{
		{Name: "ok", Gating: true, Steps: [][]string{{"fine"}}},
		{Name: "bad", Gating: true, Steps: [][]string{{"boom"}}},
		{Name: "report", Gating: false, Steps: [][]string{{"diag"}}},
	}
	results := runAssertSuiteLanes(lanes, suiteNow())
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
	if err := runCIAssertSuite("e2e", []string{"loki"}, false); err == nil {
		t.Error("a failing gating lane must fail the suite")
	}
	seamLaneRunner(t, map[string]int{})
	if err := runCIAssertSuite("e2e", []string{"loki"}, false); err != nil {
		t.Errorf("all-passing lanes must succeed, got %v", err)
	}
}

// An unknown lane name must be an ERROR. A typo that silently shrank the battery
// is the same class of bug as a lane missing from the old collection loop.
func TestSelectLanesRejectsUnknownNames(t *testing.T) {
	all := assertSuiteLanes("e2e")
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
	lanes := assertSuiteLanes("e2e")
	wantRegion := map[string]bool{"health-workflow": true, "broad-pat": true, "team-write": true}
	for _, l := range lanes {
		hasRegion := false
		for _, s := range l.Steps {
			for _, a := range s {
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
	for _, l := range assertSuiteLanes("") {
		for _, s := range l.Steps {
			for _, a := range s {
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
func TestAssertSuiteLanesNameRealVerbs(t *testing.T) {
	registered := map[string]bool{}
	for _, c := range ciCmd().Commands() {
		registered[c.Name()] = true
	}
	for _, l := range assertSuiteLanes("e2e") {
		if len(l.Steps) == 0 {
			t.Errorf("lane %s has no steps — it would pass having run nothing", l.Name)
		}
		for _, s := range l.Steps {
			if !registered[s[0]] {
				t.Errorf("lane %s invokes `llz ci %s`, which is not a registered verb", l.Name, s[0])
			}
		}
	}
}

// Every lane needs a rationale. The old YAML carried per-lane comments explaining
// what each proved; losing that in the move to Go would be a real regression in
// reviewability, so it is enforced rather than hoped for.
func TestEveryLaneDocumentsWhatItProves(t *testing.T) {
	for _, l := range assertSuiteLanes("e2e") {
		if len(strings.TrimSpace(l.Why)) < 40 {
			t.Errorf("lane %s has no meaningful Why — a failing lane must carry its own rationale", l.Name)
		}
	}
	if countGating(assertSuiteLanes("e2e")) == 0 {
		t.Fatal("the battery gates on nothing — it would always pass")
	}
}

func TestAppendLaneSummariesWritesDeliverables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", path)

	appendLaneSummaries([]laneResult{
		{Lane: suiteLane{Name: "alert-eval", SummaryTitle: "alert-eval — live rule evaluation"}, Output: "FIRING: x\n"},
		{Lane: suiteLane{Name: "loki"}, Output: "not a deliverable"},
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
	appendLaneSummaries([]laneResult{{Lane: suiteLane{SummaryTitle: "x"}, Output: "y"}})
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(t.TempDir(), "nope", "deep", "summary.md"))
	appendLaneSummaries([]laneResult{{Lane: suiteLane{SummaryTitle: "x"}, Output: "y"}})
}
