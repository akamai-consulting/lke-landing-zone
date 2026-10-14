package assertplatform

// cobra_argocomparisons.go — the CLI surface for argocomparisons.
//
// Split from argocomparisons.go so an extension directory shows its commands at a
// glance: every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"github.com/spf13/cobra"
)

func ArgoComparisonsCmd() *cobra.Command {
	var namespace string
	cmd := &cobra.Command{
		Use:   "assert-argo-comparisons",
		Short: "fail when any platform Application's comparison ERRORED, whatever its sync status claims",
		Long: "Sweeps every Argo CD Application for a ComparisonError/InvalidSpecError\n" +
			"condition and fails when a platform-owned one has it. An Application whose\n" +
			"comparison failed KEEPS ITS PREVIOUS sync status, so `Synced` on such an app\n" +
			"describes an earlier desired state and selfHeal never fires — the report names\n" +
			"that combination explicitly instead of leaving two facts in tension.\n\n" +
			"Read-only and single-shot: one `kubectl get applications`, no polling and no\n" +
			"writes, so it is safe to point at production. `llz ci converge` grades the same\n" +
			"condition but self-heals with cluster writes while it polls, which is why\n" +
			"asking it this question is not an option. Fails closed when the apiserver does\n" +
			"not answer, and when the namespace holds no Applications at all.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return assertArgoComparisons(namespace)
		},
	}
	cmd.Flags().StringVar(&namespace, "namespace", "argocd", "namespace holding the Argo CD Applications")
	return cmd
}
