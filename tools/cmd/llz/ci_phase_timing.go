package main

// ci_phase_timing.go implements `llz ci phase-mark` / `phase-report` — the
// always-on phase-timeline instrumentation (docs/designs/e2e-instrumentation.md).
//
// The e2e workflow drops a boundary MARK at each phase transition (before the
// cluster apply, after it, before/after apl-core install, converge, asserts, …);
// every mark appends {label, ts_ms} to a shared per-job log ($LLZ_PHASE_LOG,
// pointed at $RUNNER_TEMP so it persists across a job's steps). At the end of the
// job `phase-report` reads the log, computes the duration of each consecutive
// interval, prints a table to $GITHUB_STEP_SUMMARY, and writes a machine-readable
// JSON timeline that the job uploads as an artifact — so a run self-documents
// where its time went and two runs (e.g. HA-on vs HA-off) are diffable without
// log archaeology. Marks are boundaries: N marks yield N-1 intervals, each
// labeled by the mark that OPENS it; the final mark just closes the last one.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// nowMilli is the wall clock in unix-millis, seamed for deterministic tests.
var nowMilli = func() int64 { return time.Now().UnixMilli() }

// phaseLogPath resolves the shared per-job marks log: $LLZ_PHASE_LOG, else a
// stable temp path so a bare invocation still works (it just won't survive across
// steps without the env pointing at $RUNNER_TEMP).
func phaseLogPath(override string) string {
	if override != "" {
		return override
	}
	if p := os.Getenv("LLZ_PHASE_LOG"); p != "" {
		return p
	}
	return filepath.Join(os.TempDir(), "llz-phases.jsonl")
}

type phaseMark struct {
	Label string `json:"label"`
	TsMs  int64  `json:"ts_ms"`
}

type phaseInterval struct {
	Phase     string  `json:"phase"`
	StartMs   int64   `json:"start_ms"`
	EndMs     int64   `json:"end_ms"`
	DurationS float64 `json:"duration_s"`
}

func ciPhaseMarkCmd() *cobra.Command {
	var logPath string
	c := &cobra.Command{
		Use:   "phase-mark <label>",
		Short: "record a phase-boundary timestamp into the shared per-job timeline log",
		Long: "Appends {label, ts_ms} to $LLZ_PHASE_LOG (the shared per-job marks log). The\n" +
			"e2e workflow drops one at each phase boundary; `llz ci phase-report` turns the\n" +
			"marks into a duration timeline. Cheap and side-effect-free beyond the append.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return appendPhaseMark(phaseLogPath(logPath), args[0], nowMilli())
		},
	}
	c.Flags().StringVar(&logPath, "log", "", "marks log path (default $LLZ_PHASE_LOG or a temp file)")
	return c
}

func appendPhaseMark(path, label string, tsMs int64) error {
	b, err := json.Marshal(phaseMark{Label: label, TsMs: tsMs})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open phase log %s: %w", path, err)
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, string(b)); err != nil {
		return fmt.Errorf("write phase log %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "::notice::phase-mark %q\n", label)
	return nil
}

func ciPhaseReportCmd() *cobra.Command {
	var logPath, out, title string
	c := &cobra.Command{
		Use:   "phase-report",
		Short: "turn the phase-mark timeline into a $GITHUB_STEP_SUMMARY table + JSON artifact",
		Long: "Reads the shared marks log, computes each consecutive interval's duration,\n" +
			"writes a table to $GITHUB_STEP_SUMMARY, and (with --out) a JSON timeline for\n" +
			"upload as an artifact so runs are diffable. Best-effort: a missing/empty log\n" +
			"is a no-op note, never an error (an early-failed job may have few marks).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runPhaseReport(phaseLogPath(logPath), out, title)
		},
	}
	c.Flags().StringVar(&logPath, "log", "", "marks log path (default $LLZ_PHASE_LOG or a temp file)")
	c.Flags().StringVar(&out, "out", "", "write the JSON timeline here (for artifact upload)")
	c.Flags().StringVar(&title, "title", "phase timeline", "heading for the step-summary table")
	return c
}

func runPhaseReport(logPath, out, title string) error {
	marks, err := readPhaseMarks(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "::warning::phase-report: %v — skipping (no timeline)\n", err)
		return nil
	}
	intervals := computePhaseIntervals(marks)
	table := renderPhaseTable(title, intervals)
	fmt.Print(table)
	if err := appendGHAFile("GITHUB_STEP_SUMMARY", strings.TrimRight(table, "\n")); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::phase-report: step-summary write failed (ignored): %v\n", err)
	}
	if out != "" {
		b, _ := json.MarshalIndent(intervals, "", "  ")
		if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", out, err)
		}
		fmt.Fprintf(os.Stderr, "phase timeline written to %s\n", out)
	}
	return nil
}

func readPhaseMarks(path string) ([]phaseMark, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var marks []phaseMark
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m phaseMark
		if json.Unmarshal([]byte(line), &m) == nil && m.Label != "" {
			marks = append(marks, m)
		}
	}
	if len(marks) == 0 {
		return nil, fmt.Errorf("no marks in %s", path)
	}
	return marks, nil
}

// computePhaseIntervals turns boundary marks into labeled intervals. Marks are
// sorted by timestamp (steps append in order, but a defensive sort keeps the
// timeline monotonic); interval i spans mark[i]→mark[i+1], labeled by mark[i].
// A single mark yields no interval.
func computePhaseIntervals(marks []phaseMark) []phaseInterval {
	sort.SliceStable(marks, func(i, j int) bool { return marks[i].TsMs < marks[j].TsMs })
	var out []phaseInterval
	for i := 0; i+1 < len(marks); i++ {
		start, end := marks[i].TsMs, marks[i+1].TsMs
		out = append(out, phaseInterval{
			Phase:     marks[i].Label,
			StartMs:   start,
			EndMs:     end,
			DurationS: float64(end-start) / 1000.0,
		})
	}
	return out
}

func renderPhaseTable(title string, intervals []phaseInterval) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s\n\n", title)
	if len(intervals) == 0 {
		b.WriteString("_(no phase intervals recorded)_\n")
		return b.String()
	}
	b.WriteString("| phase | duration |\n|---|---|\n")
	var total float64
	for _, iv := range intervals {
		fmt.Fprintf(&b, "| %s | %s |\n", iv.Phase, fmtDuration(iv.DurationS))
		total += iv.DurationS
	}
	fmt.Fprintf(&b, "| **total** | **%s** |\n", fmtDuration(total))
	return b.String()
}

// ciCollectTimingCmd bundles the end-of-phase collection into one call so the
// workflow sites stay one line each (the inline-bash the untestable-loc guard
// counts): mkdir the output dir, optionally gather kubelet image-pull durations
// and the apl-operator helmfile logs, then write the phase-timeline report. All
// best-effort — a collection failure is a note, never a non-zero exit.
func ciCollectTimingCmd() *cobra.Command {
	var dir, title string
	var imagePulls, aplOperator bool
	c := &cobra.Command{
		Use:   "collect-timing",
		Short: "gather this run's timing artifacts (phase timeline + optional image pulls / apl-operator logs) into --dir",
		Long: "One call for the end-of-job timing bundle so the workflow stays a single\n" +
			"line: makes --dir, optionally collects kubelet image-pull durations\n" +
			"(--image-pulls, needs cluster access) and the apl-operator logs\n" +
			"(--apl-operator), then writes the phase-report timeline. Best-effort.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCollectTiming(dir, title, imagePulls, aplOperator)
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "output directory for the timing artifacts (required)")
	c.Flags().StringVar(&title, "title", "phase timeline", "heading for the step-summary table")
	c.Flags().BoolVar(&imagePulls, "image-pulls", false, "also collect kubelet image-pull durations (needs cluster access)")
	c.Flags().BoolVar(&aplOperator, "apl-operator", false, "also dump the apl-operator helmfile logs")
	return c
}

func runCollectTiming(dir, title string, imagePulls, aplOperator bool) error {
	if dir == "" {
		return fmt.Errorf("collect-timing: --dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::collect-timing: mkdir %s failed (ignored): %v\n", dir, err)
	}
	if imagePulls {
		_ = runCollectImagePulls(filepath.Join(dir, "image-pulls.json"))
	}
	if aplOperator {
		logs := execCombined("kubectl", "-n", "apl-operator", "logs",
			"-l", "app.kubernetes.io/name=apl-operator", "--tail=-1")
		if err := os.WriteFile(filepath.Join(dir, "apl-operator.log"), []byte(logs), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "::warning::collect-timing: apl-operator log write failed (ignored): %v\n", err)
		}
	}
	return runPhaseReport(phaseLogPath(""), filepath.Join(dir, "phase-timeline.json"), title)
}

// fmtDuration renders seconds as a compact human string (e.g. "3m43s", "46s").
func fmtDuration(sec float64) string {
	s := int(sec + 0.5)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}
