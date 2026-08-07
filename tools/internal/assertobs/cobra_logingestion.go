package assertobs

// cobra_logingestion.go — the CLI surface for logingestion.
//
// Split from logingestion.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
	"github.com/spf13/cobra"
)

func LogIngestionCmd() *cobra.Command {
	var loki, tenant, namespaces string
	var lookback, settle, interval, limit int
	c := &cobra.Command{
		Use:   "assert-log-ingestion",
		Short: "fail unless each landing-zone namespace's pod logs are arriving in Loki",
		Long: "Queries Loki for recent log lines from each landing-zone namespace and fails if\n" +
			"any namespace has none. Covers the cluster-wide collector path — apl-core's\n" +
			"Kubernetes service discovery over pod stdout — which is DIFFERENT from the\n" +
			"OpenBao promtail sidecar that assert-openbao-audit covers, and which nothing\n" +
			"asserted.\n\n" +
			"The failure is silent by construction: a namespace dropped from discovery, a\n" +
			"relabel rule that stops emitting `namespace`, or a NetworkPolicy blocking the\n" +
			"collector all leave pods Running, Loki Ready and assert-loki color.Green while the\n" +
			"logs are simply absent. Freshness-bounded, so collection that stopped an hour\n" +
			"ago cannot pass on Loki's retained history. Read-only. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertLogIngestion(loki, tenant, cigate.SplitCSVList(namespaces), limit,
				time.Duration(lookback)*time.Minute,
				time.Duration(settle)*time.Second,
				time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&loki, "loki", defaultAuditLokiService, "the Loki gateway Service as <namespace>/<name>:<port> to port-forward to")
	c.Flags().StringVar(&tenant, "tenant", defaultCollectorTenant,
		"Loki tenant (X-Scope-OrgID) to read as — must match the COLLECTOR's tenant, "+
			"which is not the OpenBao sidecar's (see defaultCollectorTenant)")
	c.Flags().StringVar(&namespaces, "namespaces", strings.Join(defaultLogNamespaces, ","),
		"comma-separated namespaces whose pod logs must be arriving")
	c.Flags().IntVar(&lookback, "lookback", 30, "minutes of history a namespace must have logged within")
	c.Flags().IntVar(&limit, "limit", 5, "max entries to fetch per namespace (only their existence and recency is read)")
	c.Flags().IntVar(&settle, "settle", 180, "seconds to keep polling before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}
