package dependabotcoverage

// cobra_guard.go — the CLI surface for Run.
//
// Split from guard.go so the directory shows its commands at a glance: every file
// named cobra_*.go here is flag wiring and help text, and nothing else.

import (
	"github.com/spf13/cobra"
)

// Cmd is `llz ci dependabot-coverage`.
func Cmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "dependabot-coverage",
		Short: "fail when a dependency manifest is scanned by no dependabot.yml entry",
		Long: "Walks the tree for dependency manifests — go.mod, Dockerfile, action.yml,\n" +
			".github/workflows, devcontainer.json, and .tf files declaring required_providers —\n" +
			"and fails when one is neither covered by a .github/dependabot.yml entry nor listed\n" +
			"in .dependabot-coverage.yaml with a reason.\n\n" +
			"Written because three pin sets were unscanned at once and every one looked correct\n" +
			"at its own site: for the github-actions ecosystem `directory: \"/\"` covers\n" +
			".github/workflows plus a root-level action.yml and nothing else, so consolidating\n" +
			"the repo's only actions/setup-go pin into a composite action moved it out of\n" +
			"Dependabot's reach — where it went a major version stale in silence. The Dockerfile\n" +
			"bases and the modules' provider constraints had never been listed at all.\n\n" +
			"It also fails on a config entry or an exclusion that matches nothing, because\n" +
			"coverage that scans a directory the repo no longer has reads exactly like coverage\n" +
			"that works.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return Run(root, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repo root to scan")
	return c
}
