package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPhaseMarkAppendAndReport(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "phases.jsonl")

	// Three boundary marks → two intervals.
	for _, m := range []struct {
		label string
		ts    int64
	}{{"apply-cluster", 1_000}, {"apl-core-install", 900_000}, {"converge", 1_123_000}} {
		if err := appendPhaseMark(log, m.label, m.ts); err != nil {
			t.Fatalf("appendPhaseMark: %v", err)
		}
	}

	marks, err := readPhaseMarks(log)
	if err != nil {
		t.Fatalf("readPhaseMarks: %v", err)
	}
	ivs := computePhaseIntervals(marks)
	if len(ivs) != 2 {
		t.Fatalf("intervals = %d, want 2", len(ivs))
	}
	if ivs[0].Phase != "apply-cluster" || ivs[0].DurationS != 899 {
		t.Errorf("interval 0 = %+v, want apply-cluster/899s", ivs[0])
	}
	if ivs[1].Phase != "apl-core-install" || ivs[1].DurationS != 223 {
		t.Errorf("interval 1 = %+v, want apl-core-install/223s", ivs[1])
	}

	// Report writes JSON + step summary.
	summary := filepath.Join(dir, "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)
	out := filepath.Join(dir, "timeline.json")
	if err := runPhaseReport(log, out, "phase timeline"); err != nil {
		t.Fatalf("runPhaseReport: %v", err)
	}
	jb, _ := os.ReadFile(out)
	if !strings.Contains(string(jb), `"phase": "apl-core-install"`) {
		t.Errorf("JSON artifact missing phase: %s", jb)
	}
	sb, _ := os.ReadFile(summary)
	if !strings.Contains(string(sb), "apply-cluster") || !strings.Contains(string(sb), "**total**") {
		t.Errorf("step summary missing table/total: %s", sb)
	}
}

func TestComputePhaseIntervalsSortsAndSingleMark(t *testing.T) {
	// Out-of-order marks are sorted; a single mark yields no interval.
	ivs := computePhaseIntervals([]phaseMark{{"b", 200_000}, {"a", 100_000}})
	if len(ivs) != 1 || ivs[0].Phase != "a" || ivs[0].DurationS != 100 {
		t.Fatalf("sorted interval = %+v, want a/100s", ivs)
	}
	if got := computePhaseIntervals([]phaseMark{{"only", 1}}); len(got) != 0 {
		t.Errorf("single mark → %d intervals, want 0", len(got))
	}
}

func TestPhaseReportMissingLogIsNoOp(t *testing.T) {
	if err := runPhaseReport(filepath.Join(t.TempDir(), "absent.jsonl"), "", "t"); err != nil {
		t.Errorf("missing log must be a no-op, got %v", err)
	}
}

func TestFmtDuration(t *testing.T) {
	cases := map[float64]string{0: "0s", 46: "46s", 59.6: "1m00s", 60: "1m00s", 223: "3m43s"}
	for in, want := range cases {
		if got := fmtDuration(in); got != want {
			t.Errorf("fmtDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestRunCollectTiming(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "phases.jsonl")
	t.Setenv("LLZ_PHASE_LOG", log)
	_ = appendPhaseMark(log, "apl-core-install", 1000)
	_ = appendPhaseMark(log, "converge", 900_000)
	out := filepath.Join(dir, "timing")

	// image-pulls + apl-operator off → only the phase-timeline is written; no
	// kubectl needed, so no cluster access required.
	if err := runCollectTiming(out, "bootstrap timeline", false, false); err != nil {
		t.Fatalf("collect-timing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "phase-timeline.json")); err != nil {
		t.Errorf("phase-timeline.json not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "image-pulls.json")); err == nil {
		t.Error("image-pulls.json must not be written when --image-pulls is off")
	}

	if err := runCollectTiming("", "t", false, false); err == nil {
		t.Error("empty --dir must error")
	}
}
