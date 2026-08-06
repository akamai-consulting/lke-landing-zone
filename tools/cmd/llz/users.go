package main

// users.go — `llz apl user add`, the operator command that onboards a human into
// APL by creating a Keycloak user in the `otomi` realm and granting them team
// membership (the `team-<name>` role apl-core provisions) and/or the APL
// platform-admin role (`platform-admin`). Lives only under the `apl` front door — the
// top-level `llz users` alias was retired (ADR 0013 Appendix B: users is an
// APL-domain op).
//
// The onboarding DOMAIN — validate roles, create/find the user, grant roles +
// groups, invite — lives in internal/apl/identity (ADR 0013 Phase 1). This file
// keeps the CLI surface, cluster access, and Keycloak HTTP transport: it resolves
// master-realm admin creds from the in-cluster keycloak Secret, opens an
// ephemeral kubectl port-forward (keycloakConnect), builds a kcClient, and adapts
// it to identity.AdminAPI. Everything runs against the admin REST API over the
// port-forward, so it needs no external DNS/cert — only a reachable cluster (the
// ambient bootstrap kubeconfig).
//
// Team membership is granted as the `team-<name>` REALM ROLE — the value that
// lands in the OIDC `groups` claim (apl-core ships a realm-role mapper on the
// default `openid` scope). Binding on the role rather than the group means
// onboarding works even for a fresh team whose Keycloak GROUP has not been lazily
// created yet; when the group DOES exist the user is also added to it so the APL
// console reflects the membership natively.
//
// Onboarding: by default a random temporary password is generated and printed to
// the operator (masked in CI), with a forced UPDATE_PASSWORD at first login. With
// --invite the user instead receives a Keycloak set-password email (requires the
// realm's SMTP to be configured). Creating a user is cloud-mutating, so it runs
// only under --yes; a dry-run (or a run without --yes) prints the plan and stops.

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/apl/identity"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/keycloak"
	"github.com/spf13/cobra"
)

// usersAddOpts are the flags of `llz users add`.
type usersAddOpts struct {
	email     string
	username  string
	firstName string
	lastName  string
	teams     []string
	admin     bool
	invite    bool
	region    string
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
	var o usersAddOpts
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
		RunE: func(_ *cobra.Command, _ []string) error { return runUsersAdd(gopts, o) },
	}
	f := c.Flags()
	f.StringVar(&o.email, "email", "", "user's email address; also the username unless --username is given (required)")
	f.StringVar(&o.username, "username", "", "Keycloak username (default: the email)")
	f.StringVar(&o.firstName, "first-name", "", "user's given name")
	f.StringVar(&o.lastName, "last-name", "", "user's family name")
	f.StringArrayVar(&o.teams, "team", nil, "grant membership of an APL team (the team-<name> role); repeatable")
	f.BoolVar(&o.admin, "admin", false, "grant apl-core's built-in admin team role ("+identity.PlatformAdminRole+"); not the `platform-admin` role, see --help")
	f.BoolVar(&o.invite, "invite", false, "email the user a set-password link instead of printing a temporary password (needs realm SMTP)")
	f.StringVar(&o.region, "region", "", "region whose domain gives the console URL shown on success (optional)")
	return c
}

func runUsersAdd(g globalOpts, o usersAddOpts) error {
	if o.email == "" {
		return fmt.Errorf("--email is required")
	}
	username := o.username
	if username == "" {
		username = o.email
	}
	roles, err := identity.DesiredRoles(o.teams, o.admin)
	if err != nil {
		return err
	}

	// Soft typo guard: a --team not declared in spec.teams is allowed (it may have
	// been created in the console on a managed cluster), but the role-existence
	// check in identity.AddUser is authoritative — surface the mismatch early.
	warnUndeclaredTeams(o.teams)

	fmt.Fprintf(os.Stderr, "→ add APL user %q (username %q) with roles %v in realm %s\n", o.email, username, roles, keycloakRealm)
	if g.dryRun || !g.yes {
		how := "generate a temporary password"
		if o.invite {
			how = "email a set-password link"
		}
		fmt.Fprintf(os.Stderr, "  onboarding: %s\n", how)
		fmt.Fprintln(os.Stderr, "  (dry-run — re-run with --yes to create the user)")
		return nil
	}

	user := k8sSecretField(keycloakNS, keycloakAdminSecret, "username")
	pass := k8sSecretField(keycloakNS, keycloakAdminSecret, "password")
	if user == "" || pass == "" {
		return fmt.Errorf("admin creds not readable from %s/%s (keys username/password) — is your kubectl context the target cluster?", keycloakNS, keycloakAdminSecret)
	}

	hc := &http.Client{Timeout: 30 * time.Second}
	base, token, cleanup, err := keycloakConnect(hc, user, pass, time.Sleep)
	if err != nil {
		return err
	}
	defer cleanup()
	k := &keycloak.Client{HC: hc, Base: base, Token: token, Realm: keycloakRealm}

	res, err := identity.AddUser(kcAdmin{k}, identity.AddRequest{
		Username:  username,
		Email:     o.email,
		FirstName: o.firstName,
		LastName:  o.lastName,
		Roles:     roles,
		Invite:    o.invite,
	})
	if err != nil {
		return err
	}
	reportUserAdd(o, username, res)
	return nil
}

// reportUserAdd prints the outcome: the add-only note + any non-fatal group
// warnings, what was granted, how the user signs in, and (temp-password path) the
// one-time password on stdout (masked in CI).
func reportUserAdd(o usersAddOpts, username string, res identity.AddResult) {
	if !res.Created {
		fmt.Fprintf(os.Stderr, "  user %q already existed — added roles only (password left unchanged)\n", username)
	}
	for _, w := range res.GroupWarnings {
		fmt.Fprintf(os.Stderr, "  ⚠ %s\n", w)
	}

	verb := "created"
	if !res.Created && !o.invite {
		verb = "updated"
	}
	fmt.Fprintf(os.Stderr, "✓ %s APL user %q (roles: %s)\n", verb, username, strings.Join(res.Roles, ", "))
	if console := consoleURLFor(o.region); console != "" {
		fmt.Fprintf(os.Stderr, "  console: %s\n", console)
	}
	switch {
	case o.invite:
		fmt.Fprintf(os.Stderr, "  a set-password email was sent to %s\n", o.email)
	case res.TempPassword != "":
		maskGHA(res.TempPassword)
		fmt.Fprintln(os.Stderr, "  temporary password (must be changed at first login):")
		fmt.Println(res.TempPassword) // value to stdout so it can be captured; diagnostics went to stderr
	}
}

// warnUndeclaredTeams notes any --team not present in spec.teams (best-effort;
// the spec may be unavailable, e.g. a managed cluster whose teams live only in
// the console). It is a hint, not a gate — role existence is checked for real.
func warnUndeclaredTeams(teams []string) {
	if len(teams) == 0 {
		return
	}
	declared := map[string]bool{}
	for _, t := range specTeams() {
		declared[t.Name] = true
	}
	if len(declared) == 0 {
		return // no spec to compare against — say nothing
	}
	for _, t := range teams {
		t = strings.TrimSpace(t)
		if t != "" && !declared[t] {
			fmt.Fprintf(os.Stderr, "  ⚠ team %q is not in spec.teams — proceeding only if the realm role team-%s exists\n", t, t)
		}
	}
}

// consoleURLFor best-effort resolves the APL console URL for the success message
// from --region: the env's domainSuffix (self-installed) or the in-cluster
// discovered domain (managed). Returns "" when no region is given or nothing can
// be resolved (the URL is a convenience, not required for the operation).
func consoleURLFor(region string) string {
	if region == "" {
		return ""
	}
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return ""
	}
	e, ok := lz.Env(region)
	if !ok {
		return ""
	}
	domain := e.Cluster.Bootstrap.DomainSuffix
	if domain == "" && e.Cluster.Bootstrap.ManagedAppPlatform {
		domain = discoverManagedDomain()
	}
	if domain == "" {
		return ""
	}
	return "https://console." + domain
}

// ── Keycloak admin transport (kcClient) → identity.AdminAPI ───────────────────
//
// The user/role REST methods below stay in package main: they ride the shared
// kcClient transport (do/decodeJSON, keycloakConnect) that the ci keycloak
// commands also use. kcAdmin presents them as identity.AdminAPI so the onboarding
// domain can drive them without knowing about HTTP or port-forwards.

// kcAdmin adapts *kcClient to identity.AdminAPI. The user/role REST methods use
// identity's wire types, so the adapter is pure delegation — it exists to expose
// the exported interface method set over kcClient's unexported methods
// (findGroupID/addUserToGroup are shared with the ci keycloak commands and keep
// their existing names).
type kcAdmin struct{ k *keycloak.Client }

func (a kcAdmin) FindRealmRole(name string) (*identity.Role, error)     { return a.k.FindRealmRole(name) }
func (a kcAdmin) EnsureUser(rep identity.UserRep) (string, bool, error) { return a.k.EnsureUser(rep) }
func (a kcAdmin) AssignRealmRoles(uid string, r []identity.Role) error {
	return a.k.AssignRealmRoles(uid, r)
}
func (a kcAdmin) FindGroupID(name string) (string, error)  { return a.k.FindGroupID(name) }
func (a kcAdmin) AddUserToGroup(uid, gid string) error     { return a.k.AddUserToGroup(uid, gid) }
func (a kcAdmin) SendUpdatePasswordEmail(uid string) error { return a.k.SendUpdatePasswordEmail(uid) }

// findRealmRole returns the realm role representation for an EXACT name, or nil
// when the realm has no such role.

// ensureUser creates the user, returning (id, created). On a 409 (already exists)
// it looks the user up by username and returns (id, false) so the caller can
// grant roles add-only without clobbering the existing password.

// findUserByUsername returns the id of the user with an EXACT username, or "".

// assignRealmRoles grants the given realm roles to the user (idempotent —
// re-granting an already-held role is a no-op 204). A nil/empty slice is a no-op.

// sendUpdatePasswordEmail triggers Keycloak's execute-actions email for the user,
// carrying the UPDATE_PASSWORD action (the set-password invite link). Requires
// the realm's SMTP to be configured.
