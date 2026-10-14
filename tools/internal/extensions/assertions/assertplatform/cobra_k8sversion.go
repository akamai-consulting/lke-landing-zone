package assertplatform

// cobra_k8sversion.go — the CLI surface for k8sversion.
//
// Split from k8sversion.go for the reason cobra_aplversion.go is split from its
// lane: every file named cobra_*.go here is flag wiring and help text, and nothing
// else.

import "github.com/spf13/cobra"

func K8sVersionCmd() *cobra.Command {
	var env string
	c := &cobra.Command{
		Use:   "assert-k8s-version",
		Short: "fail fast when the spec pins an LKE-Enterprise version this Linode account cannot build",
		Long: "Asks the ACCOUNT which LKE-Enterprise versions it may create\n" +
			"(/v4beta/lke/tiers/enterprise/versions) and fails when the deployment's\n" +
			"cluster.k8sVersion is not among them, naming the versions that are.\n\n" +
			"Availability is per-account: a version another account can build, or one\n" +
			"that was valid an hour ago, says nothing about this one. Run as a\n" +
			"front-loaded preflight so a retired pin fails in seconds rather than ~15\n" +
			"minutes into the cluster apply. It runs in the job apply-cluster depends\n" +
			"on, so it stops that apply; the object-storage and database roots are\n" +
			"independent jobs and still run.\n\n" +
			"Warns and PASSES when the API is unreachable, the token lacks the scope, or\n" +
			"the catalog comes back in a shape this was not measured against — a build\n" +
			"must not be blocked on a question nobody could ask.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return assertK8sVersion(env) },
	}
	c.Flags().StringVar(&env, "env", "", "deployment whose spec pin to check (e.g. prod)")
	return c
}
