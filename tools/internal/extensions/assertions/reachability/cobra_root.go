package reachability

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/spf13/cobra"
)

// cobra_root.go — the `llz <verb>` flag set, moved out of package main.
//
// An extension owns its own command; main owns the tree. These are ROOT-level
// verbs rather than `llz ci` ones, which changes nothing about the rule.

func VerifyCmd() *cobra.Command {
	var o VerifyOpts
	c := &cobra.Command{
		Use:   "verify",
		Short: "post-bootstrap acceptance snapshot (SSH wiring, platform apps, ESO) — read-only",
		Long: "Read-only validation of a freshly-bootstrapped apl-core cluster against the\n" +
			"current kubectl context: the ArgoCD SSH repository Secret + known_hosts, the\n" +
			"repo-server handshake, platform Applications Synced+Healthy, apl-git-config\n" +
			"pointed at the external HTTPS repo, OpenBao seal status, and the ESO store.\n" +
			"It does not wait — re-run if a check is just mid-reconcile.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return RunVerify(cliopts.Global.DryRun, o) },
	}
	c.Flags().StringVar(&o.SSHSourceHost, "ssh-source-host", "", "SSH source-of-truth host to check for (e.g. a self-hosted Git host); empty skips the SSH-source checks")
	return c
}
