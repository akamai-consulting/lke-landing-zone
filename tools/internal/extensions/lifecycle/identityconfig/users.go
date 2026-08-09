package identityconfig

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
// ephemeral kubectl port-forward (Connect), builds a kcClient, and adapts
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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/apl/identity"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghsecret"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/keycloak"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"
)

// UserAddOpts are the flags of `llz users add`.
type UserAddOpts struct {
	Email     string
	Username  string
	FirstName string
	LastName  string
	Teams     []string
	Admin     bool
	Invite    bool
	Region    string
}

func RunUserAdd(dryRun, yes bool, o UserAddOpts) error {
	if o.Email == "" {
		return fmt.Errorf("--email is required")
	}
	username := o.Username
	if username == "" {
		username = o.Email
	}
	roles, err := identity.DesiredRoles(o.Teams, o.Admin)
	if err != nil {
		return err
	}

	// Soft typo guard: a --team not declared in spec.teams is allowed (it may have
	// been created in the console on a managed cluster), but the role-existence
	// check in identity.AddUser is authoritative — surface the mismatch early.
	warnUndeclaredTeams(o.Teams)

	fmt.Fprintf(os.Stderr, "→ add APL user %q (username %q) with roles %v in realm %s\n", o.Email, username, roles, keycloakRealm)
	if dryRun || !yes {
		how := "generate a temporary password"
		if o.Invite {
			how = "email a set-password link"
		}
		fmt.Fprintf(os.Stderr, "  onboarding: %s\n", how)
		fmt.Fprintln(os.Stderr, "  (dry-run — re-run with --yes to create the user)")
		return nil
	}

	user := kube.SecretFieldOf(keycloakNS, AdminSecret, "username")
	pass := kube.SecretFieldOf(keycloakNS, AdminSecret, "password")
	if user == "" || pass == "" {
		return fmt.Errorf("admin creds not readable from %s/%s (keys username/password) — is your kubectl context the target cluster?", keycloakNS, AdminSecret)
	}

	hc := &http.Client{Timeout: 30 * time.Second}
	base, token, cleanup, err := Connect(hc, user, pass, time.Sleep)
	if err != nil {
		return err
	}
	defer cleanup()
	k := &keycloak.Client{HC: hc, Base: base, Token: token, Realm: keycloakRealm}

	res, err := identity.AddUser(kcAdmin{k}, identity.AddRequest{
		Username:  username,
		Email:     o.Email,
		FirstName: o.FirstName,
		LastName:  o.LastName,
		Roles:     roles,
		Invite:    o.Invite,
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
func reportUserAdd(o UserAddOpts, username string, res identity.AddResult) {
	if !res.Created {
		fmt.Fprintf(os.Stderr, "  user %q already existed — added roles only (password left unchanged)\n", username)
	}
	for _, w := range res.GroupWarnings {
		fmt.Fprintf(os.Stderr, "  ⚠ %s\n", w)
	}

	verb := "created"
	if !res.Created && !o.Invite {
		verb = "updated"
	}
	fmt.Fprintf(os.Stderr, "✓ %s APL user %q (roles: %s)\n", verb, username, strings.Join(res.Roles, ", "))
	if console := consoleURLFor(o.Region); console != "" {
		fmt.Fprintf(os.Stderr, "  console: %s\n", console)
	}
	switch {
	case o.Invite:
		fmt.Fprintf(os.Stderr, "  a set-password email was sent to %s\n", o.Email)
	case res.TempPassword != "":
		ghsecret.Mask(res.TempPassword)
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
	for _, t := range SpecTeams() {
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
		domain = DiscoverManagedDomain()
	}
	if domain == "" {
		return ""
	}
	return "https://console." + domain
}

// ── Keycloak admin transport (kcClient) → identity.AdminAPI ───────────────────
//
// The user/role REST methods below stay in internal/cli: they ride the shared
// kcClient transport (do/decodeJSON, Connect) that the ci keycloak
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
