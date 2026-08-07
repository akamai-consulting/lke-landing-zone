package converge

// cobra_aplpipeline.go — the CLI surface for aplpipeline.
//
// Split from aplpipeline.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func WaitAplPipelineCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "wait-apl-pipeline",
		Short: "block until apl-operator's helmfile brings argocd/kyverno/cert-manager up (terraform local-exec body)",
		Long: "Native port of null_resource.apl_pipeline_ready's local-exec heredoc. Writes\n" +
			"KUBECONFIG_RAW to a tempfile, then for each platform prerequisite polls until\n" +
			"the resource EXISTS (kubectl wait errors immediately on NotFound) and waits\n" +
			"for its real-readiness condition: Argo CD application-controller\n" +
			"(readyReplicas), the Kyverno admission controller (Available), and the\n" +
			"cert-manager webhook (Available). FAILS LOUD on any timeout (convergence\n" +
			"contract — no soft-fail), dumping apl-operator pods + logs when a resource\n" +
			"never appears. Reads KUBECONFIG_RAW.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIWaitAplPipeline() },
	}
}
