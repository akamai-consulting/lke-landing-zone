package main

// users.go — `llz users add`, the operator command that onboards a human into
// APL by creating a Keycloak user in the `otomi` realm and granting them team
// membership (the `team-<name>` role apl-core provisions) and/or the APL
// platform-admin role (`team-admin`).
//
// It reuses the same access path as `llz ci keycloak-configure`: an ephemeral
// kubectl port-forward to the Keycloak pod (keycloakConnect), master-realm admin
// creds from the in-cluster keycloak/platform-admin-credentials Secret, and the
// kcClient admin-REST wrapper. Everything runs against the admin REST API over
// the port-forward, so it needs no external DNS/cert — only a reachable cluster
// (the ambient bootstrap kubeconfig).
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
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/spf13/cobra"
)

// platformAdminRole is the built-in APL platform-admin realm role in the otomi
// realm. `--admin` grants it; it is not a spec.teams entry.
const platformAdminRole = "team-admin"

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

func usersCmd() *cobra.Command {
	s := &cobra.Command{
		Use:   "users",
		Short: "manage APL (Keycloak) users",
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
			"  --admin         grant the APL platform-admin role (" + platformAdminRole + ").\n" +
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
	f.BoolVar(&o.admin, "admin", false, "grant the APL platform-admin role ("+platformAdminRole+")")
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
	roles, err := desiredRoles(o)
	if err != nil {
		return err
	}

	// Soft typo guard: a --team not declared in spec.teams is allowed (it may have
	// been created in the console on a managed cluster), but the role-existence
	// check below is authoritative — surface the mismatch early as a hint.
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
	k := &kcClient{hc: hc, base: base, token: token, realm: keycloakRealm}

	return applyUserAdd(k, o, username, roles)
}

// applyUserAdd is the cluster-touching core: validate the roles exist, create (or
// find) the user, grant the roles + any matching groups, and report. Split out
// from runUsersAdd so it is testable against a fake Keycloak admin API.
func applyUserAdd(k *kcClient, o usersAddOpts, username string, roles []string) error {
	// Validate every role exists BEFORE creating the user, so a typo/unprovisioned
	// team fails fast without leaving an access-less orphan.
	reps := make([]kcRole, 0, len(roles))
	for _, rn := range roles {
		rep, err := k.findRealmRole(rn)
		if err != nil {
			return fmt.Errorf("look up realm role %q: %w", rn, err)
		}
		if rep == nil {
			hint := "apl-core has not provisioned that team yet — declare it in spec.teams and run `llz render`, or create it in the APL console"
			if rn == platformAdminRole {
				hint = "the platform-admin role is missing — is this a converged APL cluster?"
			}
			return fmt.Errorf("realm role %q does not exist: %s", rn, hint)
		}
		reps = append(reps, *rep)
	}

	// Create the user. Temp-password path sets an inline temporary credential +
	// forced UPDATE_PASSWORD; invite path creates the user password-less and emails
	// the set-password action afterwards.
	tempPassword := ""
	rep := kcUserRep{
		Username:        username,
		Email:           o.email,
		FirstName:       o.firstName,
		LastName:        o.lastName,
		Enabled:         true,
		RequiredActions: []string{"UPDATE_PASSWORD"},
	}
	if o.invite {
		rep.EmailVerified = false
	} else {
		// Temp-password login: mark the email verified so the user can sign in and
		// change the password without an SMTP round-trip.
		rep.EmailVerified = true
		tempPassword = randomPassword()
		rep.Credentials = []kcCredential{{Type: "password", Value: tempPassword, Temporary: true}}
	}

	uid, created, err := k.ensureUser(rep)
	if err != nil {
		return fmt.Errorf("create user %q: %w", username, err)
	}
	if !created {
		fmt.Fprintf(os.Stderr, "  user %q already existed — adding roles only (password left unchanged)\n", username)
		tempPassword = "" // don't imply we set one
	}

	// Grant the realm roles (idempotent), and add to any same-named group that
	// already exists so the console shows native membership.
	if err := k.assignRealmRoles(uid, reps); err != nil {
		return fmt.Errorf("grant roles %v to %q: %w", roles, username, err)
	}
	for _, rn := range roles {
		gid, err := k.findGroupID(rn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠ could not look up group %q (role granted regardless): %v\n", rn, err)
			continue
		}
		if gid == "" {
			continue // group not created yet — the role alone carries the groups claim
		}
		if err := k.addUserToGroup(uid, gid); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠ could not add %q to group %q (role granted regardless): %v\n", username, rn, err)
		}
	}

	if o.invite {
		if err := k.sendUpdatePasswordEmail(uid); err != nil {
			return fmt.Errorf("user %q created and roles granted, but the set-password email failed (is realm SMTP configured? use the default temp-password flow instead): %w", username, err)
		}
	}

	reportUserAdd(o, username, roles, tempPassword)
	return nil
}

// reportUserAdd prints the success summary: what was granted, how the user signs
// in, and (temp-password path) the one-time password on stdout (masked in CI).
func reportUserAdd(o usersAddOpts, username string, roles []string, tempPassword string) {
	verb := "created"
	if tempPassword == "" && !o.invite {
		verb = "updated"
	}
	fmt.Fprintf(os.Stderr, "✓ %s APL user %q (roles: %s)\n", verb, username, strings.Join(roles, ", "))
	if console := consoleURLFor(o.region); console != "" {
		fmt.Fprintf(os.Stderr, "  console: %s\n", console)
	}
	switch {
	case o.invite:
		fmt.Fprintf(os.Stderr, "  a set-password email was sent to %s\n", o.email)
	case tempPassword != "":
		maskGHA(tempPassword)
		fmt.Fprintln(os.Stderr, "  temporary password (must be changed at first login):")
		fmt.Println(tempPassword) // value to stdout so it can be captured; diagnostics went to stderr
	}
}

// desiredRoles resolves the deduped set of realm roles to grant from --team/--admin.
// At least one is required — a user with no team has no APL access.
func desiredRoles(o usersAddOpts) ([]string, error) {
	seen := map[string]bool{}
	var roles []string
	add := func(r string) {
		if !seen[r] {
			seen[r] = true
			roles = append(roles, r)
		}
	}
	for _, t := range o.teams {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		add("team-" + t)
	}
	if o.admin {
		add(platformAdminRole)
	}
	if len(roles) == 0 {
		return nil, fmt.Errorf("specify at least one --team <name> or --admin (a user with no team has no APL access)")
	}
	return roles, nil
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

// ── kcClient user/role helpers (admin REST) ──────────────────────────────────

// kcRole is a Keycloak realm-role representation (the subset role-mapping needs).
type kcRole struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// kcCredential is an inline password credential in a user representation.
type kcCredential struct {
	Type      string `json:"type"`
	Value     string `json:"value"`
	Temporary bool   `json:"temporary"`
}

// kcUserRep is the user representation POSTed to create a user.
type kcUserRep struct {
	Username        string         `json:"username"`
	Email           string         `json:"email,omitempty"`
	FirstName       string         `json:"firstName,omitempty"`
	LastName        string         `json:"lastName,omitempty"`
	Enabled         bool           `json:"enabled"`
	EmailVerified   bool           `json:"emailVerified"`
	RequiredActions []string       `json:"requiredActions,omitempty"`
	Credentials     []kcCredential `json:"credentials,omitempty"`
}

// findRealmRole returns the realm role representation for an EXACT name, or nil
// when the realm has no such role.
func (k *kcClient) findRealmRole(name string) (*kcRole, error) {
	resp, err := k.do(http.MethodGet, "/admin/realms/"+k.realm+"/roles/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	var role kcRole
	if err := decodeJSON(resp, &role); err != nil {
		return nil, err
	}
	if role.ID == "" {
		return nil, nil
	}
	return &role, nil
}

// ensureUser creates the user, returning (id, created). On a 409 (already exists)
// it looks the user up by username and returns (id, false) so the caller can
// grant roles add-only without clobbering the existing password.
func (k *kcClient) ensureUser(rep kcUserRep) (string, bool, error) {
	resp, err := k.do(http.MethodPost, "/admin/realms/"+k.realm+"/users", rep)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated:
		uid := path.Base(resp.Header.Get("Location"))
		if uid == "" || uid == "." {
			return "", true, fmt.Errorf("no Location header on created user")
		}
		return uid, true, nil
	case http.StatusConflict:
		uid, ferr := k.findUserByUsername(rep.Username)
		if ferr != nil {
			return "", false, fmt.Errorf("user exists (409) but lookup failed: %w", ferr)
		}
		if uid == "" {
			return "", false, fmt.Errorf("user reported as existing (409) but not found by username %q", rep.Username)
		}
		return uid, false, nil
	default:
		return "", false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
}

// findUserByUsername returns the id of the user with an EXACT username, or "".
func (k *kcClient) findUserByUsername(username string) (string, error) {
	resp, err := k.do(http.MethodGet, "/admin/realms/"+k.realm+"/users?exact=true&username="+url.QueryEscape(username), nil)
	if err != nil {
		return "", err
	}
	var users []struct{ ID, Username string }
	if err := decodeJSON(resp, &users); err != nil {
		return "", err
	}
	for _, u := range users {
		if u.Username == username {
			return u.ID, nil
		}
	}
	return "", nil
}

// assignRealmRoles grants the given realm roles to the user (idempotent —
// re-granting an already-held role is a no-op 204). A nil/empty slice is a no-op.
func (k *kcClient) assignRealmRoles(userID string, roles []kcRole) error {
	if len(roles) == 0 {
		return nil
	}
	resp, err := k.do(http.MethodPost, "/admin/realms/"+k.realm+"/users/"+userID+"/role-mappings/realm", roles)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("assign realm roles: HTTP %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

// sendUpdatePasswordEmail triggers Keycloak's execute-actions email for the user,
// carrying the UPDATE_PASSWORD action (the set-password invite link). Requires
// the realm's SMTP to be configured.
func (k *kcClient) sendUpdatePasswordEmail(userID string) error {
	resp, err := k.do(http.MethodPut, "/admin/realms/"+k.realm+"/users/"+userID+"/execute-actions-email", []string{"UPDATE_PASSWORD"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("execute-actions-email: HTTP %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}
