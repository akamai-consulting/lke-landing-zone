package monitoringlabel

// cobra_guard.go — the CLI surface for guard.
//
// Split from guard.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func Cmd() *cobra.Command {
	var roots []string
	cmd := &cobra.Command{
		Use:   "monitoring-label-guard",
		Short: "every ServiceMonitor/PodMonitor/PrometheusRule must carry `prometheus: system` (apl-core's selector)",
		Long: "Fails if any ServiceMonitor, PodMonitor, or PrometheusRule in the scanned\n" +
			"trees lacks the label `prometheus: system`. apl-core's Prometheus selects\n" +
			"monitoring CRs by that label; one without it is silently ignored (metrics\n" +
			"unscraped / rules unloaded / alerts never firing) — a class promtool and\n" +
			"kube-linter cannot see. Scans final YAML; run `make render-charts` first so\n" +
			"the rendered chart output (e.g. the openbao ServiceMonitor) is included.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return Run(roots) },
	}
	cmd.Flags().StringSliceVar(&roots, "root", []string{"platform-apl", "rendered"},
		"directories to scan (final YAML only; run render-charts to populate rendered/)")
	return cmd
}
