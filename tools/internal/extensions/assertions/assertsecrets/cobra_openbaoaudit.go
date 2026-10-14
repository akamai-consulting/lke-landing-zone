package assertsecrets

// cobra_openbaoaudit.go — the CLI surface for openbaoaudit.
//
// Split from openbaoaudit.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"time"

	"github.com/spf13/cobra"
)

func OpenbaoAuditCmd() *cobra.Command {
	var loki, tenant, selector string
	var lookback, settle, interval, limit int
	c := &cobra.Command{
		Use:   "assert-openbao-audit",
		Short: "fail unless OpenBao audit records are arriving in Loki (the audit-log pipeline round trip)",
		Long: "Gates the OpenBao audit-log pipeline end to end: queries Loki for audit records\n" +
			"shipped by the promtail sidecar within the lookback window, and fails if none\n" +
			"arrived. Catches the class that left the pipeline shipping NOWHERE for its whole\n" +
			"life — a lokiPushUrl naming a Service that does not exist, with a NetworkPolicy\n" +
			"egress allow pointed at the same empty namespace, agreeing with each other and\n" +
			"with nothing in the cluster. converge/health see a Running pod (promtail retries\n" +
			"a dead name forever), assert-loki proves only that Loki itself is up, and no\n" +
			"static guard can tell a correct URL from a plausible one.\n\n" +
			"Passing means the audit device is enabled and writable, the sidecar can read the\n" +
			"file, the gateway resolves, the NetworkPolicy admits the egress, and Loki\n" +
			"ingested the result. Polls for a settle budget, then exits 0 or 1. Read-only;\n" +
			"reaches Loki via an ephemeral kubectl port-forward.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertOpenbaoAudit(loki, tenant, selector, limit,
				time.Duration(lookback)*time.Minute,
				time.Duration(settle)*time.Second,
				time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&loki, "loki", defaultAuditLokiService,
		"the Loki gateway Service as <namespace>/<name>:<port> to port-forward to")
	c.Flags().StringVar(&tenant, "tenant", defaultAuditTenant,
		"Loki tenant (X-Scope-OrgID) to read as — must match the sidecar's promtail tenant_id")
	c.Flags().StringVar(&selector, "selector", defaultAuditSelector,
		"LogQL stream selector for the OpenBao audit stream")
	c.Flags().IntVar(&lookback, "lookback", 30,
		"minutes of history to require audit records within (the freshness window)")
	c.Flags().IntVar(&limit, "limit", 20, "max entries to fetch per poll (only their existence and shape is read)")
	c.Flags().IntVar(&settle, "settle", 180, "seconds to keep polling before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}
