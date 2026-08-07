package assertobs

// cobra_scrape.go — the CLI surface for scrape.
//
// Split from scrape.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
	"github.com/spf13/cobra"
)

func ScrapeTargetsCmd() *cobra.Command {
	var prom, monitors, ruleGroups string
	var settle, interval int
	c := &cobra.Command{
		Use:   "assert-scrape-targets",
		Short: "fail unless every landing-zone ServiceMonitor is scraped (up) and every PrometheusRule group is loaded",
		Long: "Gates the observability pipeline: asserts the in-cluster Prometheus has a\n" +
			"live `up` target for each landing-zone ServiceMonitor (/api/v1/targets) AND has\n" +
			"loaded each landing-zone PrometheusRule group (/api/v1/rules). Catches the\n" +
			"label/port/selector regressions that leave the CRs present but silently\n" +
			"un-scraped/un-loaded — which converge/health/assert-loki all miss. Polls for a\n" +
			"short settle budget to absorb a first-scrape race, then exits 0 (all wired) or\n" +
			"1. Read-only; reaches Prometheus via an ephemeral kubectl port-forward.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertScrapeTargets(prom,
				cigate.SplitCSVList(monitors), cigate.SplitCSVList(ruleGroups),
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&prom, "prom", "monitoring/prometheus-operated:9090",
		"the Prometheus Service as <namespace>/<name>:<port> to port-forward to")
	c.Flags().StringVar(&monitors, "monitors", strings.Join(defaultScrapeMonitors, ","),
		"comma-separated ServiceMonitors (namespace/name) that must each have an `up` target")
	c.Flags().StringVar(&ruleGroups, "rule-groups", strings.Join(defaultScrapeRuleGroups, ","),
		"comma-separated PrometheusRule group names that must each be loaded")
	c.Flags().IntVar(&settle, "settle", 180, "seconds to keep polling for the pipeline to come up before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}
