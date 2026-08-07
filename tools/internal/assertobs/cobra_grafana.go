package assertobs

// cobra_grafana.go — the CLI surface for grafana.
//
// Split from grafana.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
	"github.com/spf13/cobra"
)

func GrafanaDashboardsCmd() *cobra.Command {
	var dashboards string
	var settle, interval int
	c := &cobra.Command{
		Use:   "assert-grafana-dashboards",
		Short: "fail unless the landing-zone dashboards are present and labelled for BOTH Grafana sidecars",
		Long: "Asserts each landing-zone dashboard ConfigMap exists, carries dashboard JSON\n" +
			"that parses, and is labelled for BOTH dashboard sidecars: `grafana_dashboard: \"1\"`\n" +
			"(self-installed apl-core) and `release: grafana-dashboards` (managed App\n" +
			"Platform).\n\n" +
			"The sidecar is a label selector and nothing else. Drop or mistype the label and\n" +
			"the ConfigMap sits in the cluster forever, valid and invisible — Argo reports it\n" +
			"Synced because the resource matches git, converge is color.Green, and the dashboard is\n" +
			"simply not in Grafana. Carrying only ONE of the two labels is worse: it renders\n" +
			"on the stack you tested and vanishes on the other.\n\n" +
			"Scope: everything on this repo's side of the contract. It does NOT log into\n" +
			"Grafana to confirm rendering — those credentials are apl-core's, and needing\n" +
			"them would make the gate unrunnable where they are rotated or withheld.\n\n" +
			"Read-only. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertGrafanaDashboards(cigate.SplitCSVList(dashboards),
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&dashboards, "dashboards", strings.Join(DefaultGrafanaDashboards, ","),
		"comma-separated dashboard ConfigMaps (namespace/name) that must be present and discoverable")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}
