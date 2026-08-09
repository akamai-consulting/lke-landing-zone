package phasetiming

// cobra_phasetiming.go — the four `phase-timing` flag sets.
//
// The instrumentation is tools/internal/phasetiming, which declares the
// extension — and declares, at length, that the declaration does not fit.

import (
	"github.com/spf13/cobra"
)

func PhaseMarkCmd() *cobra.Command {
	var logPath string
	c := &cobra.Command{
		Use:   "phase-mark <label>",
		Short: "record a phase-boundary timestamp into the shared per-job timeline log",
		Long: "Appends {label, ts_ms} to $LLZ_PHASE_LOG (the shared per-job marks log). The\n" +
			"e2e workflow drops one at each phase boundary; `llz ci phase-report` turns the\n" +
			"marks into a duration timeline. Cheap and side-effect-free beyond the append.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return AppendPhaseMark(PhaseLogPath(logPath), args[0], NowMilli())
		},
	}
	c.Flags().StringVar(&logPath, "log", "", "marks log path (default $LLZ_PHASE_LOG or a temp file)")
	return c
}

func PhaseReportCmd() *cobra.Command {
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
			return RunPhaseReport(PhaseLogPath(logPath), out, title)
		},
	}
	c.Flags().StringVar(&logPath, "log", "", "marks log path (default $LLZ_PHASE_LOG or a temp file)")
	c.Flags().StringVar(&out, "out", "", "write the JSON timeline here (for artifact upload)")
	c.Flags().StringVar(&title, "title", "phase timeline", "heading for the step-summary table")
	return c
}

func CollectTimingCmd() *cobra.Command {
	var dir, title string
	var imagePulls, aplOperator bool
	c := &cobra.Command{
		Use:   "collect-timing",
		Short: "onboard.Gather this run's timing artifacts (phase timeline + optional image pulls / apl-operator logs) into --dir",
		Long: "One call for the end-of-job timing bundle so the workflow stays a single\n" +
			"line: makes --dir, optionally collects kubelet image-pull durations\n" +
			"(--image-pulls, needs cluster access) and the apl-operator logs\n" +
			"(--apl-operator), then writes the phase-report timeline. Best-effort.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunCollectTiming(dir, title, imagePulls, aplOperator)
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "output directory for the timing artifacts (required)")
	c.Flags().StringVar(&title, "title", "phase timeline", "heading for the step-summary table")
	c.Flags().BoolVar(&imagePulls, "image-pulls", false, "also collect kubelet image-pull durations (needs cluster access)")
	c.Flags().BoolVar(&aplOperator, "apl-operator", false, "also dump the apl-operator helmfile logs")
	return c
}

func CollectImagePullsCmd() *cobra.Command {
	var out string
	c := &cobra.Command{
		Use:   "collect-image-pulls",
		Short: "report per-image kubelet pull durations (step summary + JSON) — is a phase pull-bound?",
		Long: "Gathers the cluster's `Pulled` Events, parses each image's pull duration, and\n" +
			"writes a per-image + total table to $GITHUB_STEP_SUMMARY plus (with --out) a\n" +
			"JSON artifact. Read-only, best-effort — a kubectl/parse failure is a note.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return RunCollectImagePulls(out) },
	}
	c.Flags().StringVar(&out, "out", "", "write the JSON pull report here (for artifact upload)")
	return c
}
