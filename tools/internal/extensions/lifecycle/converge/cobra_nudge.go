package converge

// cobra_nudge.go — the CLI surface for nudge.
//
// Split from nudge.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"fmt"

	"github.com/spf13/cobra"
)

func NudgeArgoCmd() *cobra.Command {
	o := nudgeOpts{}
	c := &cobra.Command{
		Use:   "nudge-argo",
		Short: "refresh+sync Argo apps and revalidate the ClusterSecretStore post-seed (best-effort)",
		Long: "Native port of the \"Nudge Argo CD to converge secrets (post-seed)\" step.\n" +
			"First annotates each Application with argocd.argoproj.io/refresh=hard and\n" +
			"patches a fresh sync operation onto it (re-triggering any sync an earlier race\n" +
			"drove to a terminal failure). Then bumps a revalidation annotation on the\n" +
			"ClusterSecretStore and waits for it to go Ready — the converge precondition\n" +
			"only CI can assert, since only CI knows seeding just finished.\n\n" +
			"It does NOT force-sync the ExternalSecrets: the in-cluster es-store-recovery\n" +
			"reconciler lane owns that, firing on the Ready transition this bump triggers\n" +
			"(and covering PushSecrets + day-2 blips the CI half never did). Every kubectl\n" +
			"call is best-effort; this never fails the job. Defaults to the llz-secret-store\n" +
			"+ platform-bootstrap apps and the openbao store.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// cmd.Flags() includes the root's persistent flags, so this picks up
			// the global --dry-run AT EXECUTION TIME. Reading it any earlier gets
			// the pre-parse zero, which is what the retired Deps.DryRun field did
			// and why this command mutated a live cluster under --dry-run.
			//
			// THE ERROR IS NOT DISCARDABLE, and discarding it would have been the
			// retired field's defect in a new costume. GetBool returns
			// (false, err) for a flag that is not registered, and the flag lives
			// four packages away in root.go joined to this line by nothing but a
			// string literal. Rename it there and this returns false forever —
			// silently, on a verb that patches live Argo Applications. A dropped
			// `ok` producing a confident `false` is the shape this whole PR is
			// about; it must not reappear in the fix for it.
			dryRun, err := cmd.Flags().GetBool("dry-run")
			if err != nil {
				return fmt.Errorf("cannot read the global --dry-run flag (%w) — refusing to run, "+
					"because the alternative is treating an unreadable flag as \"not a dry run\" "+
					"and mutating the cluster", err)
			}
			return runCINudgeArgo(dryRun, o)
		},
	}
	c.Flags().StringSliceVar(&o.apps, "apps", defaultNudgeApps, "Argo CD Applications (argocd namespace) to refresh + sync")
	c.Flags().StringVar(&o.store, "secret-store", defaultSecretStore, "ClusterSecretStore to revalidate and wait Ready (empty skips that half)")
	c.Flags().IntVar(&o.storeTimeout, "store-timeout", 300, "seconds to wait for the ClusterSecretStore Ready condition")
	return c
}
