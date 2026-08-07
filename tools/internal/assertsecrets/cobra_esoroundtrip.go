package assertsecrets

// cobra_esoroundtrip.go — the CLI surface for esoroundtrip.
//
// Split from esoroundtrip.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
	"github.com/spf13/cobra"
)

func ESORoundTripCmd() *cobra.Command {
	var store, namespaces string
	var maxRefreshAge, settle, interval int
	c := &cobra.Command{
		Use:   "assert-eso-roundtrip",
		Short: "fail unless ExternalSecrets are still actively re-reading from OpenBao",
		Long: "Asserts the secret-delivery round trip, not an inventory: the ClusterSecretStore\n" +
			"is Ready, every platform ExternalSecret reports SecretSynced, its target Secret\n" +
			"exists with non-empty data, AND its status.refreshTime is recent.\n\n" +
			"The refreshTime check is the point. ESO materializes a Secret once and then\n" +
			"refreshes on an interval; when the READ path breaks — stale store CA, a lost\n" +
			"k8s-auth policy, a renamed KV path, a sealed OpenBao — the already-written\n" +
			"Secret keeps sitting there with its old value and every consumer keeps working.\n" +
			"converge is color.Green and the Secret exists. Only staleness of the refresh can\n" +
			"distinguish that from a healthy pipeline.\n\n" +
			"Fails closed, including on finding zero ExternalSecrets. Read-only. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertESORoundTrip(store, cigate.SplitCSVList(namespaces),
				time.Duration(maxRefreshAge)*time.Minute,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&store, "store", esoStoreName, "the ClusterSecretStore that must be Ready")
	c.Flags().StringVar(&namespaces, "namespaces", "",
		"comma-separated namespaces to check ExternalSecrets in (default: all namespaces)")
	c.Flags().IntVar(&maxRefreshAge, "max-refresh-age", 90,
		"minutes; an ExternalSecret whose status.refreshTime is older than this has stopped re-reading the backend")
	c.Flags().IntVar(&settle, "settle", 180, "seconds to keep polling before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}
