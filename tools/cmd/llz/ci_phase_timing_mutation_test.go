package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Marks stamped in the same millisecond must keep their APPEND order. SliceStable
// guarantees that only while the comparator is a strict less; a `<=` comparator
// reports equal elements as ordered and the stable insertion sort swaps them,
// silently reversing the timeline for two phases that boundary-marked together.
func TestComputePhaseIntervalsKeepsEqualTimestampsInAppendOrder(t *testing.T) {
	ivs := computePhaseIntervals([]phaseMark{
		{"first", 5_000}, {"second", 5_000}, {"third", 9_000},
	})
	if len(ivs) != 2 {
		t.Fatalf("intervals = %+v, want 2", ivs)
	}
	if ivs[0].Phase != "first" || ivs[1].Phase != "second" {
		t.Errorf("phase order = [%s %s], want [first second] (equal timestamps keep append order)",
			ivs[0].Phase, ivs[1].Phase)
	}
	if ivs[0].DurationS != 0 || ivs[1].DurationS != 4 {
		t.Errorf("durations = [%v %v], want [0 4]", ivs[0].DurationS, ivs[1].DurationS)
	}
}

// A step-summary write that SUCCEEDS must not emit the ignored-failure warning.
// That warning is the only observable of the write, so an inverted guard turned
// every healthy run into a false alarm without failing a test.
func TestPhaseReportQuietWhenTheStepSummaryWriteSucceeds(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "phases.jsonl")
	if err := appendPhaseMark(log, "apply-cluster", 1_000); err != nil {
		t.Fatal(err)
	}
	if err := appendPhaseMark(log, "converge", 61_000); err != nil {
		t.Fatal(err)
	}
	summary := filepath.Join(dir, "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)

	var err error
	errOut := captureStderr(t, func() {
		_ = captureStdout(t, func() { err = runPhaseReport(log, "", "phase timeline") })
	})
	if err != nil {
		t.Fatalf("runPhaseReport: %v", err)
	}
	if strings.Contains(errOut, "step-summary write failed") {
		t.Errorf("the write succeeded, yet: %s", errOut)
	}
	if b, _ := os.ReadFile(summary); !strings.Contains(string(b), "apply-cluster") {
		t.Errorf("step summary not actually written: %q", b)
	}
}

// Same shape for collect-timing's mkdir: a successful MkdirAll must be silent.
func TestRunCollectTimingQuietWhenMkdirSucceeds(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "phases.jsonl")
	t.Setenv("LLZ_PHASE_LOG", log)
	if err := appendPhaseMark(log, "apl-core-install", 1_000); err != nil {
		t.Fatal(err)
	}
	if err := appendPhaseMark(log, "converge", 900_000); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "timing")

	var err error
	errOut := captureStderr(t, func() {
		_ = captureStdout(t, func() { err = runCollectTiming(out, "bootstrap timeline", false, false) })
	})
	if err != nil {
		t.Fatalf("collect-timing: %v", err)
	}
	if strings.Contains(errOut, "mkdir") {
		t.Errorf("the mkdir succeeded, yet: %s", errOut)
	}
	if fi, statErr := os.Stat(out); statErr != nil || !fi.IsDir() {
		t.Errorf("--dir not created: %v", statErr)
	}
}
