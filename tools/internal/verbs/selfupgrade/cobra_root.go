package selfupgrade

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/spf13/cobra"
)

// cobra_root.go — the `llz <verb>` flag set, moved out of package main.
//
// An extension owns its own command; main owns the tree. These are ROOT-level
// verbs rather than `llz ci` ones, which changes nothing about the rule.

func SelfUpdateCmd() *cobra.Command {
	var ref, repo string
	c := &cobra.Command{
		Use:   "self-update",
		Short: "replace this llz binary with a release build (gh-authenticated, checksum-verified)",
		Long: "Downloads an llz release for this OS/arch from the template repo (via `gh`,\n" +
			"so it works against a private repo), verifies it against the\n" +
			"release SHA256SUMS, and atomically overwrites the running binary. Defaults to\n" +
			"the latest vX.Y.Z release; --dry-run reports the target without installing.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunSelfUpdate(cliopts.Global.DryRun, repo, ref)
		},
	}
	c.Flags().StringVar(&ref, "ref", "", "release to install, e.g. v0.0.39 (default: latest)")
	c.Flags().StringVar(&repo, "repo", "", "template repo to pull from (default: upstream_org/lke-landing-zone)")
	return c
}
