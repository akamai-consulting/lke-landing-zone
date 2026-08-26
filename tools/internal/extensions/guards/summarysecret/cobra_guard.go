package summarysecret

// cobra_guard.go — the CLI surface for guard.
//
// Split from guard.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func Cmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "summary-secret-guard",
		Short: "fail when a function that masks secrets writes a computed value to the job summary",
		Long: "Parses the Go tree and, for every FILE that calls `ghsecret.Mask`, requires\n" +
			"that each argument to a $GITHUB_STEP_SUMMARY append be a string literal —\n" +
			"unless the call site is registered in summaryComputedAllowed with a reason.\n" +
			"\n" +
			"Masking redacts the LOG stream; a job summary is a Markdown file GitHub\n" +
			"renders exactly as written, and Actions READ is enough to open it.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return Run(root)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root")
	return c
}
