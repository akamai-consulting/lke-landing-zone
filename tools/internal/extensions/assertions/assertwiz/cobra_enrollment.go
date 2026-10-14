package assertwiz

// cobra_enrollment.go — the CLI surface for enrollment.go. Flag wiring and help
// text only.

import (
	"time"

	"github.com/spf13/cobra"
)

// EnrollmentCmd is `llz ci assert-wiz-enrollment`.
func EnrollmentCmd() *cobra.Command {
	var region string
	var settle, interval int
	c := &cobra.Command{
		Use:   "assert-wiz-enrollment",
		Short: "fail unless the cluster is enrolled in Wiz (credential delivered, connector registered, broker Ready)",
		Long: "Reads what the Wiz enrollment chain PRODUCED and judges each link: the\n" +
			"wiz-api-token ExternalSecret is Ready, its Secret carries non-empty clientId +\n" +
			"clientToken (never printed), the registration Secret holds a connector record\n" +
			"the upstream Job only writes after the Wiz API accepted the cluster, and the\n" +
			"broker Deployment has a Ready replica (its probe is the tunnel's own /ready).\n\n" +
			"Skipping is anchored to the SPEC: with --region it skips (loudly, exit 0) only\n" +
			"when spec.components.wiz is disabled for that env; enabled-but-absent FAILS.\n" +
			"Inside `llz ci assert-suite` the lane follows the component, so a deployment\n" +
			"without Wiz shows the lane as SKIPPED rather than passed.\n\n" +
			"Polls up to --settle seconds; stops early on a failure waiting cannot fix\n" +
			"(unseeded path, missing keys). Exit 0 enrolled/skipped, 1 otherwise.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runAssertWizEnrollment(region, time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&region, "region", "", "deployment whose spec decides whether enrollment is expected")
	c.Flags().IntVar(&settle, "settle", 300, "seconds to wait for the chain to converge before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between polls")
	return c
}
