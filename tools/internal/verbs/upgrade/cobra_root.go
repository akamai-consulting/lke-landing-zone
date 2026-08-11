package upgrade

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/spf13/cobra"
)

// cobra_root.go — the `llz <verb>` flag set, moved out of package main.
//
// An extension owns its own command; main owns the tree. These are ROOT-level
// verbs rather than `llz ci` ones, which changes nothing about the rule.

func UpgradeCmd() *cobra.Command {
	var ref string
	var commit, noRender, noDoctor bool
	c := &cobra.Command{
		Use: "upgrade", Short: "copier update to a new template release (conflict-gated, re-rendered, summarized, optionally committed)",
		Long: "Updates the instance from its pinned template: `copier update` (which\n" +
			"re-records the pin in .copier-answers.yml) + apply the template's declared\n" +
			"file removals. Then gates on\n" +
			"leftover merge-conflict markers (fails loudly instead of shipping them),\n" +
			"re-runs `llz render` — the new pin is what the committed apl-values `?ref=`\n" +
			"resolves to, so they are stale by construction until it does — prints\n" +
			"a one-view summary of the churn, and — with --commit — records it as a single\n" +
			"labeled `chore(template): upgrade vX -> vY` commit so you review one diff.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return Run(cliopts.Global.DryRun, ref, commit, noRender, noDoctor)
		},
	}
	c.Flags().StringVar(&ref, "ref", "", "template release tag to update + re-pin to (default: this llz binary's version)")
	c.Flags().BoolVar(&commit, "commit", false, "stage + record the upgrade as one labeled git commit")
	c.Flags().BoolVar(&noRender, "no-render", false, "skip the post-update `llz render` (leaves apl-values pointing at the OLD ref — run `llz render` yourself)")
	c.Flags().BoolVar(&noDoctor, "no-doctor", false, "skip the post-upgrade readiness check (it is advisory and never fails the upgrade; use offline)")
	return c
}
