package tofudriver

// cobra_plan.go — the CLI surface for plan.
//
// Split from plan.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func PlanCmd() *cobra.Command {
	var out, title string
	var lines int
	c := &cobra.Command{
		Use:   "tf-plan --out <tee-file> --title <title> [--lines N] [-- terraform flags...]",
		Short: "terraform plan, teed to a file + tail appended to the step summary",
		Long: "Native port of terraform-plan.sh + terraform-summarize-plan.sh.\n" +
			"Runs `terraform plan -no-color [flags...]` with stdout+stderr combined,\n" +
			"streamed live and teed to --out. On success the last --lines lines are\n" +
			"appended to $GITHUB_STEP_SUMMARY as a fenced code block under --title\n" +
			"(skipped when the env var is unset). Flags after `--` pass through to\n" +
			"terraform. A failed plan fails the command and writes no summary.",
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			return runCITFPlan(out, title, lines, args)
		},
	}
	c.Flags().StringVar(&out, "out", "", "file to tee the plan output to")
	c.Flags().StringVar(&title, "title", "", "step-summary heading for the plan tail")
	c.Flags().IntVar(&lines, "lines", 80, "trailing lines of plan output to put in the summary")
	_ = c.MarkFlagRequired("out")
	_ = c.MarkFlagRequired("title")
	return c
}
