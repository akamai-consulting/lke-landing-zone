package wavehealth

// cobra_health.go — the CLI surface for health.
//
// Split from health.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func HealthGuardCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "wave-health-guard",
		Short: "fail when a negative-sync-wave resource kind could health-wedge the platform-bootstrap sync",
		Long: "Static guard for the PR #142 wedge class: Argo sync waves gate on per-resource\n" +
			"health, so any kind at a negative wave in platform-apl/manifest/ or\n" +
			"platform-apl/components/ must be health-inert or neutralized by a\n" +
			"resource.customizations.health override in apl-values/values.yaml. Unknown kinds at\n" +
			"negative waves fail with remediation guidance; kinds whose safety depends on a\n" +
			"values override fail if the override key is missing.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIWaveHealthGuard(root) },
	}
	cmd.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return cmd
}
