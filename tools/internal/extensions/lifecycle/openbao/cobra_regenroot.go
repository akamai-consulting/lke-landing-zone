package openbao

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/spf13/cobra"
)

// cobra_regenroot.go — the `llz <verb>` flag set, moved out of package main.
//
// An extension owns its own command; main owns the tree. These are ROOT-level
// verbs rather than `llz ci` ones, which changes nothing about the rule.

func RegenRootCmd() *cobra.Command {
	var o RegenRootOpts
	c := &cobra.Command{
		Use:   "regen-root <region>",
		Short: "quorum-regenerate root — needs you to HOLD 3 recovery keys (else use break-glass)",
		Long: "Runs the `bao operator generate-root` quorum flow against the active raft\n" +
			"leader in the cluster your kubectl context points at, reading recovery-key\n" +
			"shares in terminal raw mode (never echoed/stored).\n" +
			"\n" +
			"REQUIRES that you already HOLD 3 of the 5 recovery keys — they are printed\n" +
			"once to the bootstrap job summary and otherwise kept offline; they are NOT\n" +
			"distributed to operators. If you do not have them, do NOT use this command:\n" +
			"run the break-glass workflow (`breakglass-openbao.yml`, or `llz ci\n" +
			"bao-breakglass`) instead, which reconstitutes root from the recovery quorum\n" +
			"stored in the infra-<region> GitHub environment (OPENBAO_RECOVERY_KEY_1..3) —\n" +
			"no operator-held keys needed. See docs/runbooks/bootstrap-openbao.md.\n" +
			"\n" +
			"<region> names the infra-<region> GitHub environment for --update-gha-secret.\n" +
			"Run after a bootstrap revokes root.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			return RunRegenRoot(cliopts.Global.DryRun, a[0], o)
		},
	}
	c.Flags().BoolVar(&o.UpdateGHA, "update-gha-secret", false, "write the new root to infra-<region>.OPENBAO_ROOT_TOKEN")
	c.Flags().StringVar(&o.Repo, "repo", "", "owner/repo for gh (avoids multi-remote auto-detect failures)")
	return c
}
