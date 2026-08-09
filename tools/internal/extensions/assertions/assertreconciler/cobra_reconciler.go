package assertreconciler

// cobra_reconciler.go — the CLI surface for reconciler.
//
// Split from reconciler.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/spf13/cobra"
)

func ReconcilerCmd() *cobra.Command {
	var prom, namespace, lanes string
	var settle, interval, staleFactor int
	var checkLanes bool
	c := &cobra.Command{
		Use:   "assert-reconciler",
		Short: "fail unless the reconciler is reporting healthy (llz_reconcile_up=1), has a leader, and every enabled lane is running fresh",
		Long: "Asserts the reconciler's functional gauges in the in-cluster Prometheus:\n" +
			"llz_reconcile_up == 1 (the reconcile loop is up AND its samples succeed — a\n" +
			"pod that is Running yet failing on lost RBAC/OpenBao access reports 0) and\n" +
			"llz_reconcile_leader == 1 (a replica holds the driving Lease). Catches the\n" +
			"silently-broken-reconciler class that converge/health (pod phase) and\n" +
			"alert-eval --strict (ignores FIRING) both miss.\n\n" +
			"ALSO asserts every ENABLED lane is passing recently (--lanes, default on).\n" +
			"llz_reconcile_up is a max() across lanes, so one dead lane among nine leaves\n" +
			"it pinned at 1; and because the registry never expires a gauge, a dead lane\n" +
			"keeps serving its last-known-good sample instead of going absent. The\n" +
			"per-lane freshness series is the only thing that contradicts that, and\n" +
			"nothing consumed it: LLZReconcilerStale covers it as an ALERT, but alert-eval\n" +
			"is report-only and --strict ignores FIRING by design.\n\n" +
			"The expected lane set comes from the reconciler DEPLOYMENT's --reconcile-*\n" +
			"args, not from the metrics — a lane that is declared but never reports is\n" +
			"exactly the failure, and deriving the expectation from the gauge would make\n" +
			"it unobservable. Polls a short settle budget, then exits 0 or 1. Read-only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertReconciler(prom, namespace, checkLanes, cigate.SplitCSVList(lanes), staleFactor,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&prom, "prom", "monitoring/prometheus-operated:9090",
		"the Prometheus Service as <namespace>/<name>:<port> to port-forward to")
	c.Flags().StringVar(&namespace, "namespace", "llz-reconciler", "namespace label the reconciler gauges carry")
	c.Flags().BoolVar(&checkLanes, "lanes", true, "also assert every enabled lane has passed recently (per-lane freshness)")
	c.Flags().StringVar(&lanes, "lane-set", "",
		"comma-separated lane names to require (default: read the Deployment's --reconcile-* args)")
	c.Flags().IntVar(&staleFactor, "stale-factor", 10,
		"a lane is stale after this many of its OWN llz_reconcile_interval_seconds without a success (matches the LLZReconcilerStale rule)")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling for the reconciler to report healthy before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}
