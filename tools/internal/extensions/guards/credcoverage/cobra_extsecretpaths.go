package credcoverage

// cobra_extsecretpaths.go — the CLI surface for extsecretpaths.
//
// Split from extsecretpaths.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"os"

	"github.com/spf13/cobra"
)

func ExternalSecretPathsCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "externalsecret-paths",
		Short: "cross-validate ExternalSecret remoteRefs against OpenBao seeding + policy coverage",
		Long: "Native port of the former template-scripts/linting-and-validation/\n" +
			"validate-externalsecret-paths.py (the Makefile's externalsecret-paths-check,\n" +
			"run after render-charts). Validates every ExternalSecret remoteRef.key/\n" +
			"property in apl-values/ + $RENDER_DIR against the bootstrap-workflow and\n" +
			"ci_harbor.go seeding, then asserts the bao-configure platform-ci policy\n" +
			"covers every consumed and seeded KV v2 path.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCIExternalSecretPaths(root, os.Stdout)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repo root to validate")
	return c
}
