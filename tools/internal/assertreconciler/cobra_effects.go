package assertreconciler

// cobra_effects.go — the CLI surface for effects.
//
// Split from effects.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
	"github.com/spf13/cobra"
)

func EffectsCmd() *cobra.Command {
	var namespace, laneSet string
	var tokenMaxAge, settle, interval int
	var requireInventory bool
	c := &cobra.Command{
		Use:   "assert-reconciler-effects",
		Short: "fail unless each enabled reconciler lane's cluster invariant actually holds",
		Long: "Asserts what the reconciler lanes DO, not that they are running: exactly one\n" +
			"default StorageClass (sc-demote), a fresh llz-token-inventory ConfigMap\n" +
			"(token-inventory), and a populated firewall-controller ConfigMap\n" +
			"(cidr-firewall). A lane can report a successful pass every cycle while its\n" +
			"effect is absent — reconciled onto the wrong object, reverted by Argo, or\n" +
			"computed from empty input — and assert-reconciler --lanes cannot see that,\n" +
			"because it reads the lane's own self-report.\n\n" +
			"Each sub-check skips only when its lane is not enabled on this cluster, and\n" +
			"fails on every ambiguity otherwise. Read-only. Exit 0 all invariants hold, 1\n" +
			"otherwise.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertReconcilerEffects(namespace, cigate.SplitCSVList(laneSet), requireInventory,
				time.Duration(tokenMaxAge)*time.Minute,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&namespace, "namespace", "llz-reconciler", "the reconciler's namespace (also where llz-token-inventory lives)")
	c.Flags().StringVar(&laneSet, "lane-set", "",
		"comma-separated lanes to check effects for (default: read the Deployment's --reconcile-* args)")
	c.Flags().BoolVar(&requireInventory, "require-token-inventory", false,
		"fail when the llz-token-inventory ConfigMap has never been written (default: report the skip). "+
			"Its writer is the scheduled llz-scheduled-checks job, so on a fresh cluster absence is normal; "+
			"set this for a caller that runs the writer itself, where absence is a break")
	c.Flags().IntVar(&tokenMaxAge, "token-inventory-max-age", 180,
		"minutes; the llz-token-inventory ConfigMap must have been updated within this window")
	c.Flags().IntVar(&settle, "settle", 180, "seconds to keep polling before failing (absorbs a lane's first sweep after converge)")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}
