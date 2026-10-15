package assertobs

// cobra_alertrouting.go — the CLI surface for alertrouting.
//
// Split from alertrouting.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"os"
	"time"

	"github.com/spf13/cobra"
)

func AlertRoutingCmd() *cobra.Command {
	var am, region string
	var settle, interval int
	c := &cobra.Command{
		Use:   "assert-alert-routing",
		Short: "fail unless Alertmanager routes alerts to the receivers spec.alerting names (PagerDuty / Slack), exactly once each",
		Long: "The hop after assert-alert-delivery: Alertmanager → the receiver spec.alerting\n" +
			"declares. Reads the config Alertmanager is RUNNING (/api/v2/status) and requires\n" +
			"each expected receiver, with its routing key / webhook set (for Slack, also the\n" +
			"alertmanager-credentials Secret behind apl-core's slack_api_url_file). Then creates\n" +
			"a silence, posts silenced synthetic probe alerts — from a non-monitoring namespace\n" +
			"and with none — and reads back the receivers Alertmanager itself chose, requiring\n" +
			"EXACTLY the spec's number of PagerDuty and Slack paths per probe (a duplicate\n" +
			"fails), including an info probe that must notify nobody. Then reads every team\n" +
			"Alertmanager's running config and fails if one carries LLZ's receivers (a second\n" +
			"page). Reports, without failing, Alertmanager's failed-notification counters.\n" +
			"The silence is verified active before any probe is posted and is left to expire,\n" +
			"so the check never notifies anyone.\n\n" +
			"Expectations come from the spec (landingzone.yaml + environments/<region>.yaml in\n" +
			"the working directory), never from the cluster. An env with no receiver, or without\n" +
			"observability, is reported and exits 0. Does not prove the provider accepted an\n" +
			"event — see docs/alerting.md.\n\n" +
			"Writes only to Alertmanager's own API (one silence, short-lived probe alerts).\n" +
			"Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertAlertRouting(region, am,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&region, "region", os.Getenv("REGION"),
		"the deployment whose effective spec.alerting (landingzone.yaml merged with environments/<region>.yaml) is graded; "+
			"optional when the instance has exactly one")
	c.Flags().StringVar(&am, "alertmanager", defaultAlertmanagerSpec,
		"the Alertmanager Service as <namespace>/<name>:<port> to port-forward to")
	// 300s, not assert-alert-delivery's 120: this waits on four clocks in series —
	// ESO's refresh, the operator's reconcile, the config Secret's propagation to
	// the pod, and the config-reloader — after a fresh converge.
	c.Flags().IntVar(&settle, "settle", 300, "seconds to keep polling for the receivers before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}
