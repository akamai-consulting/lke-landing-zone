package assertsuite

// cobra_suite.go — the CLI surface for suite.
//
// Split from suite.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/spf13/cobra"
)

func Cmd() *cobra.Command {
	var region, only string
	var list bool
	c := &cobra.Command{
		Use:   "assert-suite",
		Short: "run the e2e assert battery — independent gates in parallel lanes",
		Long: "Runs the release-e2e assert battery: independent read-mostly gates fanned out as\n" +
			"parallel lanes, each lane's steps ordered and short-circuiting, with per-lane\n" +
			"::group:: output and a single pass/fail verdict.\n\n" +
			"This was ~40 lines of inline bash in the bootstrap workflow — a job runner\n" +
			"written in YAML. Three things were wrong with that: the code deciding whether\n" +
			"the battery FAILS was the only untested part of it; the lane list was written\n" +
			"twice (once to run, once to collect) so a lane could run and never be able to\n" +
			"fail the step; and the list shipped per-instance, so a new gate reached an\n" +
			"instance only when someone edited its vendored YAML. The list now exists once,\n" +
			"in tested Go, and travels with the binary.\n\n" +
			"Report-only lanes (metric-surface, alert-eval) are run and printed but never\n" +
			"gate. Exit 0 when every GATING lane passed, 1 otherwise.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return Run(region, cigate.SplitCSVList(only), list)
		},
	}
	c.Flags().StringVar(&region, "region", os.Getenv("REGION"), "deployment/region passed to the lanes that need it (defaults to $REGION)")
	c.Flags().StringVar(&only, "only", "", "comma-separated subset of lanes to run (default: all)")
	c.Flags().BoolVar(&list, "list", false, "print the lane table and exit without running anything")
	return c
}
