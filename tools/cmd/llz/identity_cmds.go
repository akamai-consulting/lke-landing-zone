package main

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/apl/identity"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cliopts"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/identityconfig"
	"github.com/spf13/cobra"
)

// identity_cmds.go — the cobra wiring for the five identity-plane lanes. The
// lanes themselves are internal/identityconfig.
//
// `usersAddCmd` is the one lane in this campaign that reads TWO fields off
// globalOpts, not one: `dryRun` and `yes`. It is the only extracted command that
// creates a human user, and it is print-what-you-would-do unless BOTH are
// satisfied — so the pair travels as two bools rather than a struct, and package
// main stays the only thing that knows globalOpts exists.

func ciKeycloakConfigureCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "keycloak-configure",
		Short: "ensure the Keycloak device-flow client for team-scoped OpenBao login (spec.teams)",
		Long: "Realm half of the team-scoped-credentials turnkey path (bao-configure owns\n" +
			"the OpenBao half). Port-forwards to the Keycloak pod, authenticates with the\n" +
			"in-cluster " + identityconfig.AdminSecret + " admin creds, then idempotently ensures a\n" +
			"single PUBLIC device-flow OIDC client (" + identityconfig.DeviceClientID + ") carrying the default `openid`\n" +
			"scope. The per-team groups + `groups` claim are apl-core's job (the native\n" +
			"team-<name> group/role from the teamConfig `llz render` emits), so this does\n" +
			"NOT create groups or mappers. Best-effort: any Keycloak failure WARNS (and\n" +
			"points at the manual runbook step) rather than failing, so it is safe in the\n" +
			"bootstrap path. No-op when spec.teams is empty.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return identityconfig.RunKeycloakConfigure(cliopts.Global.DryRun, region)
		},
	}
	c.Flags().StringVar(&region, "region", "", "region name used in operator-facing messages (required)")
	return c
}

func ciPinKeycloakGatewayAliasCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "pin-keycloak-gateway-alias",
		Short: "point keycloak.<domain> at the ingress gateway's ClusterIP inside the OpenBao pods",
		Long: "Pins a hostAliases entry on the OpenBao StatefulSet so the in-cluster JWKS\n" +
			"fetch reaches the Istio ingress gateway directly instead of hairpinning\n" +
			"through the external LoadBalancer (unsupported on LKE-E), while keeping the\n" +
			"public hostname on the wire so the wildcard certificate still verifies.\n\n" +
			"Idempotent: re-running with an unchanged ClusterIP makes no write, so it does\n" +
			"not churn the StatefulSet or restart OpenBao.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return identityconfig.RunPinGatewayAlias(region) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment name (resolves the Keycloak issuer/domain)")
	return c
}

func ciBaoConfigureCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "bao-configure",
		Short: "configure OpenBao: KV v2, Kubernetes + GitHub-OIDC auth, policies, roles, audit verify",
		Long: "Native port of configure-openbao.sh. Preflights $OPENBAO_ROOT_TOKEN (sha256\n" +
			"audit line + `token lookup` + root-policy check — without it the failure\n" +
			"mode is an unexplained cascade of 403s from every privileged call), then\n" +
			"applies the mounts/auth/policy/role sequence and verifies the declarative\n" +
			"file/ audit device is active (warns + sets BOOTSTRAP_ERRORS=true when not).\n" +
			"Idempotent: enables tolerate already-enabled, writes upsert.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return identityconfig.RunBaoConfigure(cliopts.Global.DryRun, region)
		},
	}
	c.Flags().StringVar(&region, "region", "", "region name used in operator-facing error messages (required)")
	return c
}

// aplUserCmd is `llz apl user` — the sole home of APL user management, reached as
// a leaf of the `apl` front door (aplCmd). Formerly the top-level `llz users`;
// retired there per ADR 0013 Appendix B (users → apl user).
func aplUserCmd() *cobra.Command {
	s := &cobra.Command{
		Use:   "user",
		Short: "onboard & manage App Platform (Keycloak) users",
		Long: "Create and manage the human users of the APL platform — Keycloak users in\n" +
			"the `otomi` realm. `add` onboards a user and grants them team membership\n" +
			"(the team-<name> role apl-core provisions from spec.teams) and/or the APL\n" +
			"platform-admin role. It reaches Keycloak over an ephemeral kubectl\n" +
			"port-forward using the in-cluster admin creds, so it needs a reachable\n" +
			"cluster (the ambient bootstrap kubeconfig) but no external DNS. Distinct\n" +
			"from `llz secrets` (GitHub secrets) and `llz openbao` (OpenBao KV).",
	}
	s.AddCommand(usersAddCmd())
	return s
}

func usersAddCmd() *cobra.Command {
	var o identityconfig.UserAddOpts
	c := &cobra.Command{
		Use:   "add --email <addr> (--team <name>... | --admin) [--invite] [--yes]",
		Short: "create an APL user in Keycloak and grant team/admin access (--yes)",
		Long: "Create a Keycloak user in the `otomi` realm and grant it access.\n" +
			"\n" +
			"Access (at least one required):\n" +
			"  --team <name>   grant membership of an APL team — the team-<name> realm\n" +
			"                  role (repeatable). The team must already exist (declared\n" +
			"                  in spec.teams and rendered, or created in the APL console).\n" +
			"  --admin         grant apl-core's built-in admin TEAM role\n" +
			"                  (" + identity.PlatformAdminRole + "). Note this is NOT the `platform-admin`\n" +
			"                  role the built-in otomi-admin account carries, so it does\n" +
			"                  not currently confer `llz openbao login --team <name>` —\n" +
			"                  use --team for that. See docs/runbooks/openbao-team-login.md.\n" +
			"\n" +
			"Onboarding:\n" +
			"  default         generate a random TEMPORARY password, print it (masked in\n" +
			"                  CI), and force UPDATE_PASSWORD at first console login.\n" +
			"  --invite        instead email the user a Keycloak set-password link\n" +
			"                  (requires the realm's SMTP to be configured).\n" +
			"\n" +
			"Idempotent: if the user already exists the missing roles are added and the\n" +
			"password is left untouched (add-only). Creating a user is cloud-mutating, so\n" +
			"it runs only with --yes; without it (or with --dry-run) the plan is printed.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return identityconfig.RunUserAdd(cliopts.Global.DryRun, cliopts.Global.Yes, o)
		},
	}
	f := c.Flags()
	f.StringVar(&o.Email, "email", "", "user's email address; also the username unless --username is given (required)")
	f.StringVar(&o.Username, "username", "", "Keycloak username (default: the email)")
	f.StringVar(&o.FirstName, "first-name", "", "user's given name")
	f.StringVar(&o.LastName, "last-name", "", "user's family name")
	f.StringArrayVar(&o.Teams, "team", nil, "grant membership of an APL team (the team-<name> role); repeatable")
	f.BoolVar(&o.Admin, "admin", false, "grant apl-core's built-in admin team role ("+identity.PlatformAdminRole+"); not the `platform-admin` role, see --help")
	f.BoolVar(&o.Invite, "invite", false, "email the user a set-password link instead of printing a temporary password (needs realm SMTP)")
	f.StringVar(&o.Region, "region", "", "region whose domain gives the console URL shown on success (optional)")
	return c
}
