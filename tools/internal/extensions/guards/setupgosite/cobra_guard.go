package setupgosite

// cobra_guard.go — the CLI surface for Run.
//
// Split from guard.go so the directory shows its commands at a glance: every file
// named cobra_*.go here is flag wiring and help text, and nothing else.

import (
	"github.com/spf13/cobra"
)

// Cmd is `llz ci setup-go-sole-site`.
func Cmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "setup-go-sole-site",
		Short: "fail when a workflow sets up Go without ./.github/actions/setup-llz",
		Long: "Fails when any Actions YAML outside .github/actions/setup-llz/action.yml uses\n" +
			"actions/setup-go, and when that composite itself stops using it.\n\n" +
			"The composite was extracted because the same setup-go block appeared 13 times,\n" +
			"making its pinned SHA a 13-site sweep to bump and letting the build flags drift.\n" +
			"A second copy had since reappeared in release-e2e-lane.yml pinned a full major\n" +
			"version away from the composite's, in a job running the same functional script\n" +
			"that llz-release.yml runs through the composite. Every such site is individually\n" +
			"well-formed, so only the relation between them is checkable.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return Run(root, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repo root to scan")
	return c
}
