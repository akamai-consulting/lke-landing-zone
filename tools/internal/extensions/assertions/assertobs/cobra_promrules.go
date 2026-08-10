package assertobs

// cobra_promrules.go — the CLI surface for promrules.
//
// Split from promrules.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func HealthPromRulesCmd() *cobra.Command {
	var prom string
	c := &cobra.Command{
		Use:   "health-prom-rules",
		Short: "report PrometheusRule groups with evaluation errors (warn-only)",
		Long: "Queries Prometheus /api/v1/rules and reports any rule carrying a lastError to\n" +
			"the step summary + ::warning:: annotations — evaluation failures (missing\n" +
			"metric, label-join mistake) that promtool's syntax check cannot catch. Reads\n" +
			"REGION for the report headings.\n\n" +
			"Warn-only by design, but it no longer passes VACUOUSLY: an unreachable\n" +
			"Prometheus is an error, not a clean skip. It previously looked for the pod in\n" +
			"llz-observability — which holds the LLZ ServiceMonitor/PrometheusRule CRs,\n" +
			"while apl-core's Prometheus runs in monitoring — so it took its skip path on\n" +
			"every run and nothing ever validated the live rules.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIHealthPromRules(prom) },
	}
	c.Flags().StringVar(&prom, "prom", "monitoring/prometheus-operated:9090",
		"Prometheus to query, as <namespace>/<service-or-pod>:<port>")
	return c
}
