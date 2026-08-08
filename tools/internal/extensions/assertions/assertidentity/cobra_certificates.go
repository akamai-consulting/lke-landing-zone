package assertidentity

// cobra_certificates.go — the CLI surface for certificates.
//
// Split from certificates.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"time"

	"github.com/spf13/cobra"
)

func CertificatesCmd() *cobra.Command {
	var minDays, settle, interval int
	c := &cobra.Command{
		Use:   "assert-certificates",
		Short: "fail unless every cert-manager Certificate is Ready and not expiring inside the renewal window",
		Long: "Asserts every cert-manager Certificate is Ready and has more than --min-days\n" +
			"left. The signal already existed — the reconciler publishes\n" +
			"llz_certificates_not_ready and LLZCertificatesNotReady alerts on it — but\n" +
			"alert-eval is report-only and --strict ignores FIRING, so a Certificate stuck\n" +
			"not-Ready reds nothing.\n\n" +
			"Reads the objects, not the gauge: the gauge is a COUNT, so a gate built on it\n" +
			"fails with \"3\" and sends the operator hunting on a cluster e2e tears down\n" +
			"minutes later. Not-Ready (broken issuance) and expiring-soon (broken renewal)\n" +
			"are reported separately because the remedies share nothing.\n\n" +
			"Fails closed, including on finding zero Certificates — this platform issues its\n" +
			"own CA chain, so none means cert-manager never reconciled. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertCertificates(minDays,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().IntVar(&minDays, "min-days", 14, "fail a Ready Certificate with fewer than this many days left (renewal should have happened well inside it)")
	c.Flags().IntVar(&settle, "settle", 180, "seconds to keep polling before failing (absorbs first issuance on a fresh cluster)")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}
