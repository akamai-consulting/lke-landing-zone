package assertobjstore

// cobra_assertobjstore.go — the `llz ci assert-obj-roundtrip` flag set.
//
// The assertion is tools/internal/extensions/lifecycle/assertobjstore, which declares the extension.
//
// NO Deps STRUCT — the third extraction to need none. Every shell-out was a
// `kubectl get`, and internal/kubectlprobe already exports Exec with the identical
// signature, so clause three of the Deps rule ("is it already injectable
// elsewhere?") answered it.

import (
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
)

func AssertObjRoundTripCmd() *cobra.Command {
	var only, keyPrefix, root, env string
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
			"push; a read-only probe passes on a bucket that has gone read-only. Exit 0 / 1.\n\n" +
			"Both consumers are OPTIONAL apl-core apps, so on a managed cluster the set is\n" +
			"narrowed to the ones spec.cluster.bootstrap.managedApps declares — an instance\n" +
			"that never enabled Harbor has no credential Secret for it, and failing that\n" +
			"forever is how a scheduled gate gets switched off. Skips are printed. An\n" +
			"unreadable spec checks EVERYTHING rather than narrowing on evidence it does not\n" +
			"have, and --only overrides the narrowing entirely.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			if env == "" {
				env = os.Getenv("REGION")
			}
			return Run(cigate.SplitCSVList(only), keyPrefix, root, env,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&only, "only", "", "comma-separated consumers to check (overrides the spec narrowing)")
	c.Flags().StringVar(&root, "root", ".", "instance root holding landingzone.yaml + environments/")
	c.Flags().StringVar(&env, "env", "", "deployment whose managedApps decide which consumers are checked (default: $REGION)")
	c.Flags().StringVar(&keyPrefix, "key-prefix", "llz-roundtrip-probe/", "object key prefix for the probe object")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}
