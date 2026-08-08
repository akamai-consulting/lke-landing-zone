package assertobjstore

// cobra_assertobjstore.go — the `llz ci assert-obj-roundtrip` flag set.
//
// The assertion is tools/internal/assertobjstore, which declares the extension.
//
// NO Deps STRUCT — the third extraction to need none. Every shell-out was a
// `kubectl get`, and internal/kubectlprobe already exports Exec with the identical
// signature, so clause three of the Deps rule ("is it already injectable
// elsewhere?") answered it.

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
)

func AssertObjRoundTripCmd() *cobra.Command {
	var only, keyPrefix string
	var settle, interval int
	c := &cobra.Command{
		Use:   "assert-obj-roundtrip",
		Short: "fail unless Loki and Harbor can write to, and read back from, their object storage",
		Long: "Writes a small object, reads it back and compares the bytes, then deletes it —\n" +
			"once per consumer, using THAT consumer's own credential Secret and its own\n" +
			"endpoint and bucket as read from its live config.\n\n" +
			"verify-object-storage lists buckets through the LINODE API and confirms they\n" +
			"exist by label. Every one of those checks passed while Loki and Harbor were both\n" +
			"returning NoSuchBucket: Linode's object-storage generations are DISJOINT\n" +
			"namespaces on different hosts, so an obj-cluster id stripped to its region puts\n" +
			"the bucket on gen-1 while the consumers address gen-2. The bucket exists, the\n" +
			"API is telling the truth, and the consumer 404s.\n\n" +
			"Only a check that speaks S3 at the CONSUMER's endpoint with the CONSUMER's\n" +
			"credential can tell those apart, and it must read back: a PUT can succeed\n" +
			"against the wrong endpoint. If a consumer's endpoint cannot be read from its\n" +
			"config this FAILS rather than deriving one — the derived endpoint is the view\n" +
			"that was already wrong.\n\n" +
			"Writes, because Loki writes chunks continuously and Harbor writes on every\n" +
			"push; a read-only probe passes on a bucket that has gone read-only. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return Run(cigate.SplitCSVList(only), keyPrefix,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&only, "only", "", "comma-separated consumers to check (default: all)")
	c.Flags().StringVar(&keyPrefix, "key-prefix", "llz-roundtrip-probe/", "object key prefix for the probe object")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}
