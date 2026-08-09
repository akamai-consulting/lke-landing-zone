package assertobs

// cobra_alertdelivery.go — the CLI surface for alertdelivery.
//
// Split from alertdelivery.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"time"

	"github.com/spf13/cobra"
)

func AlertDeliveryCmd() *cobra.Command {
	var prom, am string
	var settle, interval int
	c := &cobra.Command{
		Use:   "assert-alert-delivery",
		Short: "fail unless Prometheus has a live Alertmanager to deliver firing alerts to",
		Long: "Asserts the one alerting link nothing else covers: Prometheus → Alertmanager.\n" +
			"promtool checks rule syntax, assert-scrape-targets proves rules are LOADED, and\n" +
			"alert-eval proves they EVALUATE — but a rule can evaluate to FIRING and have\n" +
			"nowhere to go. With no Alertmanager discovered (a dropped alertmanagerConfig, a\n" +
			"NetworkPolicy, a renamed Service) the alert fires into a void: dashboards show\n" +
			"it, nobody is paged, and nothing anywhere reports an error, because firing with\n" +
			"no receivers is not a failure condition for Prometheus.\n\n" +
			"Checks /api/v1/alertmanagers has at least one ACTIVE endpoint, and that\n" +
			"Alertmanager itself answers /api/v2/status and /api/v2/alerts.\n\n" +
			"Scope: this is the link this repo owns. It does NOT assert Alertmanager\n" +
			"forwards to a human destination — apl-core owns receivers and none ship here,\n" +
			"so gating on them would fail every cluster that deliberately has none.\n\n" +
			"Does not require any alert to be firing. Read-only. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertAlertDelivery(prom, am,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&prom, "prom", "monitoring/prometheus-operated:9090",
		"the Prometheus Service as <namespace>/<name>:<port> to port-forward to")
	c.Flags().StringVar(&am, "alertmanager", defaultAlertmanagerSpec,
		"the Alertmanager Service as <namespace>/<name>:<port> to port-forward to")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}
