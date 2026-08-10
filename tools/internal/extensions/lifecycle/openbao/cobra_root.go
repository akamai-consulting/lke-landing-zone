package openbao

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/spf13/cobra"
)

// cobra_root.go — the `llz <verb>` flag set, moved out of package main.
//
// An extension owns its own command; main owns the tree. These are ROOT-level
// verbs rather than `llz ci` ones, which changes nothing about the rule.

func OpenbaoCmd() *cobra.Command {
	s := &cobra.Command{
		Use:   "openbao",
		Short: "read/write secrets in the OpenBao cluster(s) by HA role (KV v2)",
		Long: "Reads from / writes to the OpenBao cluster(s) over the KV v2 HTTP API,\n" +
			"addressed by HA role. `set` dual-writes to an active+standby pair, or\n" +
			"single-writes a standalone deployment (when no standby is configured).\n" +
			"Auth + addresses come from OPENBAO_ADDR_{ACTIVE,STANDBY} and\n" +
			"OPENBAO_TOKEN_{ACTIVE,STANDBY} (or OPENBAO_TOKEN); OPENBAO_NAMESPACE is\n" +
			"optional.\n" +
			"\n" +
			"When OPENBAO_ADDR_ACTIVE is unset on a standalone deployment, get/set open\n" +
			"an ephemeral `kubectl port-forward` to the leader pod in the cluster your\n" +
			"kubectl context points at (TLS verify skipped on the loopback tunnel) — so\n" +
			"a plain `llz openbao get/set` with just a token Just Works, no address to\n" +
			"wire. Set OPENBAO_ADDR_ACTIVE to override. Distinct from `llz secrets`\n" +
			"(which manages GitHub secrets).\n" +
			"\n" +
			"CREDENTIALS — prefer a team-scoped token over root. For day-2 get/set, run\n" +
			"`eval \"$(llz openbao login --team <name>)\"` to mint a short-lived,\n" +
			"attributed, least-privilege OPENBAO_TOKEN via Keycloak OIDC — no root token.\n" +
			"The root token (OPENBAO_ROOT_TOKEN) is reserved for `exec` (auth/policy\n" +
			"admin) and break-glass; get/set/exec warn when they fall back to it.",
	}
	// `exec` is a thin pass-through to `bao` inside the cluster. SetInterspersed
	// (false) makes cobra STOP flag-parsing at the first positional (the bao
	// subcommand: write / kv / read), so bao's own flags (-f, -format=json, -)
	// reach bao instead of cobra rejecting them ("unknown shorthand flag") — no
	// `--` separator required. llz's global --dry-run still parses because it
	// precedes `openbao`; an explicit `llz openbao exec -- …` also still works.
	execCmd := &cobra.Command{
		Use:   "exec [--] <bao args...>",
		Short: "run a bao command in the cluster via kubectl exec (day-2 auth/policy admin; needs OPENBAO_ROOT_TOKEN)",
		Args:  cobra.MinimumNArgs(1),
		RunE:  func(_ *cobra.Command, a []string) error { return RunExec(cliopts.Global.DryRun, a) },
	}
	execCmd.Flags().SetInterspersed(false)

	s.AddCommand(
		&cobra.Command{
			Use:   "get <active|standby> <secret/path> <key>",
			Short: "read one field from a cluster by HA role (value to stdout)",
			Args:  cobra.ExactArgs(3),
			RunE:  func(_ *cobra.Command, a []string) error { return RunGet(a[0], a[1], a[2]) },
		},
		&cobra.Command{
			Use:   "set <secret/path> <key=value>...",
			Short: "dual-write to active+standby, or single-write a standalone (--yes); rollback + hash-verify",
			Args:  cobra.MinimumNArgs(2),
			RunE: func(_ *cobra.Command, a []string) error {
				return RunSet(cliopts.Global.DryRun, cliopts.Global.Yes, a[0], a[1:])
			},
		},
		execCmd,
		OpenbaoLoginCmd(),
		RegenRootCmd(),
	)
	return s
}
func OpenbaoLoginCmd() *cobra.Command {
	var o TeamLoginOpts
	c := &cobra.Command{
		Use:   "login --team <name>",
		Short: "mint a team-scoped OpenBao token via Keycloak OIDC (no root token)",
		Long: "Human-operator auth for team-scoped writes. Runs an OAuth 2.0 Device\n" +
			"Authorization Grant against the APL Keycloak realm, then swaps the id_token\n" +
			"for a short-lived OpenBao token via the `keycloak` jwt mount (provisioned by\n" +
			"`llz ci bao-configure` from spec.teams). The token carries only the team's\n" +
			"`<name>-writer` policy, so `llz openbao set` no longer needs the root token.\n" +
			"Prints `export OPENBAO_TOKEN=…` to stdout — load it with:\n" +
			"  eval \"$(llz openbao login --team <name>)\"\n" +
			"The issuer is derived from the region's cluster.bootstrap.domainSuffix (the\n" +
			"otomi realm); on a Managed App Platform instance (no domainSuffix) it is\n" +
			"discovered from the cluster (otomi/otomi-api), so no --issuer is needed —\n" +
			"pass --issuer only to override. Requires kubectl reach to the target cluster\n" +
			"(KUBECONFIG): it port-forwards OpenBao for the token exchange. See\n" +
			"docs/designs/team-scoped-credentials.md.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return RunTeamLogin(o) },
	}
	f := c.Flags()
	f.StringVar(&o.Team, "team", "", "team name == OpenBao keycloak role == spec.teams entry (required)")
	f.StringVar(&o.Region, "region", "", "spec env whose domainSuffix derives the Keycloak issuer (optional if the spec has one env)")
	f.StringVar(&o.Issuer, "issuer", "", "Keycloak realm issuer URL override (e.g. https://keycloak.<domain>/realms/otomi)")
	f.StringVar(&o.ClientID, "client-id", "", "Keycloak OIDC device-flow client id (default: $OPENBAO_OIDC_CLIENT_ID or 'llz')")
	return c
}
