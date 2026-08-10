package assertobs

// cobra_alerteval.go — the CLI surface for alerteval.
//
// Split from alerteval.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func AlertEvalCmd() *cobra.Command {
	var match, prom, summary string
	var strict bool
	cmd := &cobra.Command{
		Use:   "alert-eval",
		Short: "evaluate deployed PrometheusRule alert exprs against the live Prometheus (find never-fire / false-positive rules)",
		Long: "Reads the PrometheusRule CRs off the cluster and runs each alert expr through\n" +
			"the in-cluster Prometheus /api/v1/query (via an ephemeral kubectl port-forward).\n" +
			"Classifies each as FIRING / ARMED / DEAD? / BROKEN so you can catch alerts that\n" +
			"reference a non-existent metric (promtool passes, but they never fire) or that\n" +
			"trip on a healthy cluster. Read-only.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIAlertEval(match, prom, summary, strict) },
	}
	cmd.Flags().StringVar(&match, "match", "^(LLZ|OTel|Loki|Grafana|Harbor|SupportPlane|OpenBao)",
		"RE2 regex the alert name must match (default: the landing-zone alert families)")
	cmd.Flags().StringVar(&prom, "prom", "monitoring/prometheus-operated:9090",
		"the Prometheus Service as <namespace>/<name>:<port> to port-forward to")
	cmd.Flags().StringVar(&summary, "summary", "",
		"when set, append a fenced verdict block under this title to $GITHUB_STEP_SUMMARY")
	cmd.Flags().BoolVar(&strict, "strict", false, "exit 1 if any alert is DEAD? or BROKEN")
	return cmd
}
