package credcoverage

// cobra_coverage.go — the CLI surface for coverage.
//
// Split from coverage.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func CoverageGuardCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "credential-coverage-guard",
		Short: "fail when an instance workflow uses a credential nothing measures",
		Long: "Static gate on credential-observability drift. Every `secrets.NAME` an\n" +
			"instance workflow consumes must either be MEASURED — by expiry (ghPATTargets,\n" +
			"or the Linode account enumeration) or by GitHub write time (ghSecretTargets) —\n" +
			"or be REGISTERED in credCoverageExempt with a kind and a reason.\n\n" +
			"Coverage is read from those lists directly rather than restated here, so the\n" +
			"way to satisfy this guard for a real credential is to measure it. Unregistered\n" +
			"secrets fail, and so do registry entries no workflow uses any more.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCICredentialCoverageGuard(root) },
	}
	c.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return c
}
