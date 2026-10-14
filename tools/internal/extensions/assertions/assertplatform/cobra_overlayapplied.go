package assertplatform

// cobra_overlayapplied.go — the CLI surface for overlayapplied.
//
// Split from overlayapplied.go so an extension directory shows its commands at a
// glance: every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"github.com/spf13/cobra"
)

func OverlayAppliedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "assert-overlay-applied",
		Short: "fail unless what the apl-overlay declares is what the live objects actually carry",
		Long: "Reads each mapped apl-overlay `_rawValues` path back out of the CLUSTER and, when\n" +
			"the live object disagrees, server-dry-runs the same change to learn WHY. A field\n" +
			"the API server fixes at create time can never be applied to an object that\n" +
			"already exists — and because Argo CD computes its diff by dry-run-applying the\n" +
			"desired state, that rejection produces no diff, so the Application reads Synced\n" +
			"and selfHeal never fires. The rejection is per object, so one unappliable field\n" +
			"silently discards every other change to it.\n\n" +
			"Read-only: a server dry run persists nothing, and the capability model classifies\n" +
			"it as a cluster read. Safe to point at production, which is where this class of\n" +
			"failure lives — a fresh cluster creates each object in its final shape and never\n" +
			"meets it.\n\n" +
			"Covers only the paths in clusterspec.OverlayFields(); every other declared path\n" +
			"is printed as UNCHECKED with its count, never counted as a pass.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return assertOverlayApplied()
		},
	}
	return cmd
}
