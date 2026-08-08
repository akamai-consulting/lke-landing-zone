package assertobs

// cobra_prommetrics.go — the CLI surface for prommetrics.
//
// Split from prommetrics.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func PromMetricsCmd() *cobra.Command {
	var match, prom string
	cmd := &cobra.Command{
		Use:   "prom-metrics",
		Short: "list in-cluster Prometheus metric names matching a regex (metric-name discovery)",
		Long: "Queries the in-cluster Prometheus (via an ephemeral kubectl port-forward)\n" +
			"for every scraped metric name and prints those matching --match. Use it to\n" +
			"discover the real exporter metric names (loki_*, otelcol_*, harbor_*) before\n" +
			"writing an error-rate/saturation alert — promtool validates syntax, not that\n" +
			"a metric exists. Read-only; best-effort (exit 0 even on no matches).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIPromMetrics(match, prom) },
	}
	cmd.Flags().StringVar(&match, "match", ".", "RE2 regex the metric name must match")
	cmd.Flags().StringVar(&prom, "prom", "monitoring/prometheus-operated:9090",
		"the Prometheus Service as <namespace>/<name>:<port> to port-forward to")
	return cmd
}
