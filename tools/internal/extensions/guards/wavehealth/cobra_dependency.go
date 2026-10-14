package wavehealth

// cobra_dependency.go — the CLI surface for dependency.
//
// Split from dependency.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func DependencyGuardCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "wave-dependency-guard",
		Short: "fail when a workload syncs at or before the ExternalSecret that provides a Secret it hard-depends on",
		Long: "Static guard for the #163 wedge class: Argo sync waves gate on per-resource\n" +
			"health, so a Deployment/StatefulSet/DaemonSet that hard-references (non-optional)\n" +
			"a Secret produced by an ExternalSecret at a LATER sync-wave can never go Healthy\n" +
			"— it blocks its wave forever and starves every later-wave ExternalSecret in the\n" +
			"platform-bootstrap sync. The workload's wave must be strictly greater than the\n" +
			"ExternalSecret's. Mark the reference optional: true to opt out (the pod then\n" +
			"starts without the Secret).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIWaveDependencyGuard(root) },
	}
	cmd.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return cmd
}
