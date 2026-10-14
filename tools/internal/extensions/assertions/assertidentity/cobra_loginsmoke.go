package assertidentity

// cobra_loginsmoke.go — the CLI surface for loginsmoke.
//
// Split from loginsmoke.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func TeamLoginSmokeCmd() *cobra.Command {
	var region, team string
	c := &cobra.Command{
		Use:   "team-login-smoke",
		Short: "e2e: validate the team-scoped OpenBao write + ESO read paths (no browser)",
		Long: "Browser-free end-to-end check of the team-scoped credential paths: provisions\n" +
			"a throwaway Keycloak user in the team-<name> group, mints an id_token via a\n" +
			"direct-grant client (the same groups claim the device flow would carry),\n" +
			"exchanges it at OpenBao's keycloak mount, then asserts a write to the team's\n" +
			"subtree SUCCEEDS and a write outside it is DENIED (403). Asserts the\n" +
			"platform-admin path: a user carrying only platform-admin (not the team group)\n" +
			"can likewise mint the team's writer token and write. Finally asserts the READ\n" +
			"half: the external-secrets SA, via the `eso` Kubernetes-auth role, can read\n" +
			"the team-written key (the <team>-reader policy) but is denied an uncovered\n" +
			"path. Tears down the users + client. Meant for the e2e lane (needs cluster\n" +
			"access + a converged apl-core Keycloak). See docs/runbooks/openbao-team-login.md.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runTeamLoginSmoke(region, team) },
	}
	c.Flags().StringVar(&region, "region", "", "region whose domainSuffix gives the public Keycloak URL (required)")
	c.Flags().StringVar(&team, "team", "", "team to validate (default: the first spec.teams entry)")
	return c
}
