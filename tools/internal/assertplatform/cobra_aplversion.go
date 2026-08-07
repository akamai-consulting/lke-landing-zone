package assertplatform

// cobra_aplversion.go — the CLI surface for aplversion.
//
// Split from aplversion.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func AplVersionCmd() *cobra.Command {
	var env string
	c := &cobra.Command{
		Use:   "assert-apl-version",
		Short: "fail fast when the spec pins an apl-core chart version the landing zone no longer supports",
		Long: "Resolves the apl-core chart version exactly as `llz ci bootstrap-cluster` does\n" +
			"(spec.cluster.bootstrap.aplChartVersion for the deployment, else the baked\n" +
			"default) and fails when it is older than " + MinSupportedAplChartVersion + ".\n\n" +
			"Run as a front-loaded preflight so an unsupported pin fails in seconds rather\n" +
			"than wedging apl-operator (missing apl-sops-secrets) and leaving the cluster\n" +
			"with no external-secrets operator — both ~2h into the bootstrap.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return assertAplVersion(env) },
	}
	c.Flags().StringVar(&env, "env", "", "deployment whose spec pin to check (e.g. prod); empty checks the baked default only")
	return c
}
