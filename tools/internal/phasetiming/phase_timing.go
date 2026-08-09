package phasetiming

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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

// NowMilli is the wall clock in unix-millis, seamed for deterministic tests.
var NowMilli = func() int64 { return time.Now().UnixMilli() }

// PhaseLogPath resolves the shared per-job marks log: $LLZ_PHASE_LOG, else a
// stable temp path so a bare invocation still works (it just won't survive across
// steps without the env pointing at $RUNNER_TEMP).
func PhaseLogPath(override string) string {
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

func AppendPhaseMark(path, label string, tsMs int64) error {
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

func RunPhaseReport(logPath, out, title string) error {
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

func RunCollectTiming(dir, title string, imagePulls, aplOperator bool) error {
	if dir == "" {
		return fmt.Errorf("collect-timing: --dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::collect-timing: mkdir %s failed (ignored): %v\n", dir, err)
	}
	if imagePulls {
		_ = RunCollectImagePulls(filepath.Join(dir, "image-pulls.json"))
	}
	if aplOperator {
		logs := execCombined("kubectl", "-n", "apl-operator", "logs",
			"-l", "app.kubernetes.io/name=apl-operator", "--tail=-1")
		if err := os.WriteFile(filepath.Join(dir, "apl-operator.log"), []byte(logs), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "::warning::collect-timing: apl-operator log write failed (ignored): %v\n", err)
		}
	}
	return RunPhaseReport(PhaseLogPath(""), filepath.Join(dir, "phase-timeline.json"), title)
}

// fmtDuration renders seconds as a compact human string (e.g. "3m43s", "46s").
func fmtDuration(sec float64) string {
	s := int(sec + 0.5)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}

// ── localised helpers: copies, not seams ────────────────────────────────────
//
// NEITHER OF THESE BECAME A Deps FIELD, and both were checked against the
// three-clause rule (can the package already do this with a grant it holds? is it
// a pure function? is it already injectable elsewhere?).
//
// appendGHAFile is a file append to a path the ENVIRONMENT names. It is not a
// capability another package owns — internal/envtopology reached the same
// conclusion and keeps its own copy as the real default, precisely because a
// no-op version turns every test that asserts on the summary into a tautology.
//
// execCombined is a kubectl shell-out, and internal/kubectlprobe already exports
// Exec with the signature it needs; clause three answered it, as it did for
// argocd-diagnostics and assert-objstore. Best-effort by design: a diagnostic that
// fails because its log fetch failed would bury the timing it was run to collect.

// appendGHAFile appends lines to the GitHub Actions command file named by envVar
// (GITHUB_OUTPUT / GITHUB_ENV / GITHUB_STEP_SUMMARY). Outside Actions the variable
// is unset and the write is skipped, keeping the commands runnable from a
// workstation.
func appendGHAFile(envVar string, lines ...string) error {
	path := os.Getenv(envVar)
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open $%s: %w", envVar, err)
	}
	for _, l := range lines {
		if _, err := fmt.Fprintln(f, l); err != nil {
			f.Close()
			return fmt.Errorf("write $%s: %w", envVar, err)
		}
	}
	return f.Close()
}

// execCombined runs a command and returns its combined output, ignoring failure.
func execCombined(name string, args ...string) string {
	out, _ := kubectlprobe.Exec(name, args...)
	return string(out)
}
