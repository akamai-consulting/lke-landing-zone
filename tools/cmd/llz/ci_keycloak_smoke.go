package main

// ci_keycloak_smoke.go — `llz ci team-login-smoke`, an END-TO-END validation of
// the team-scoped OpenBao write path, browser-free. It exercises the exact chain
// that is otherwise only E2E-gated: apl-core provisions the `team-<name>` group +
// realm role → a member's OIDC token carries `groups: [team-<name>]` → OpenBao's
// `keycloak` role binds it → the `<name>-writer` policy scopes the write.
//
// It does NOT drive the device-flow browser (that UX is generic OAuth 2.0 device
// grant, already httptest-covered). Instead it mints the same id_token via a
// throwaway PUBLIC direct-grant client — apl's realm allows direct access grants —
// so a scripted CI run gets a real, apl-shaped groups claim with no browser.
//
// Everything Keycloak-side runs against the PUBLIC realm URL (keycloak.<domain>)
// so the id_token's `iss` matches OpenBao's configured oidc_discovery_url; only
// the token exchange + the write assertions ride the OpenBao port-forward. All
// created objects (client, user) are torn down; the in-subtree smoke secret is
// left (the team token has no delete capability — harmless on a throwaway e2e KV).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
	"github.com/spf13/cobra"
)

func ciTeamLoginSmokeCmd() *cobra.Command {
	var region, team string
	c := &cobra.Command{
		Use:   "team-login-smoke",
		Short: "e2e: validate the team-scoped OpenBao write path end-to-end (no browser)",
		Long: "Browser-free end-to-end check of the team-scoped write path: provisions a\n" +
			"throwaway Keycloak user in the team-<name> group, mints an id_token via a\n" +
			"direct-grant client (the same groups claim the device flow would carry),\n" +
			"exchanges it at OpenBao's keycloak mount, then asserts a write to the team's\n" +
			"subtree SUCCEEDS and a write outside it is DENIED (403). Tears down the user +\n" +
			"client. Meant for the e2e lane (needs cluster access + a converged apl-core\n" +
			"Keycloak). See docs/runbooks/openbao-team-login.md.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runTeamLoginSmoke(gopts, region, team) },
	}
	c.Flags().StringVar(&region, "region", "", "region whose domainSuffix gives the public Keycloak URL (required)")
	c.Flags().StringVar(&team, "team", "", "team to validate (default: the first spec.teams entry)")
	return c
}

// smokeTargets resolves the public Keycloak base URL + the team name/subtree to
// validate, from the spec.
func smokeTargets(region, teamFlag string) (base, team, subtree string, err error) {
	lz, e := clusterspec.LoadInstance(".")
	if e != nil {
		return "", "", "", fmt.Errorf("load spec: %w", e)
	}
	teams := lz.Spec.Teams
	if len(teams) == 0 {
		return "", "", "", fmt.Errorf("no spec.teams declared — nothing to validate")
	}
	pick := teams[0]
	if teamFlag != "" {
		found := false
		for _, t := range teams {
			if t.Name == teamFlag {
				pick, found = t, true
				break
			}
		}
		if !found {
			return "", "", "", fmt.Errorf("team %q not in spec.teams", teamFlag)
		}
	}
	env, ok := lz.Env(region)
	if !ok || env.Cluster.Bootstrap.DomainSuffix == "" {
		return "", "", "", fmt.Errorf("region %q has no cluster.bootstrap.domainSuffix — can't form the public Keycloak URL", region)
	}
	return "https://keycloak." + env.Cluster.Bootstrap.DomainSuffix, pick.Name, pick.OpenbaoSubtree, nil
}

func runTeamLoginSmoke(g globalOpts, region, teamFlag string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	base, team, subtree, err := smokeTargets(region, teamFlag)
	if err != nil {
		return err
	}
	user := k8sSecretField(keycloakNS, keycloakAdminSecret, "username")
	pass := k8sSecretField(keycloakNS, keycloakAdminSecret, "password")
	if user == "" || pass == "" {
		return fmt.Errorf("admin creds not readable from %s/%s (keys username/password)", keycloakNS, keycloakAdminSecret)
	}
	fmt.Fprintf(os.Stderr, "→ team-login smoke: team %q, subtree %q, realm %s at %s\n", team, subtree, keycloakRealm, base)

	hc := &http.Client{Timeout: 30 * time.Second}
	adminTok, err := keycloakAdminToken(hc, base, user, pass)
	if err != nil {
		return fmt.Errorf("keycloak admin token: %w", err)
	}
	k := &kcClient{hc: hc, base: base, token: adminTok, realm: keycloakRealm}

	group := "team-" + team
	gid, err := k.findGroupID(group)
	if err != nil {
		return fmt.Errorf("look up group %s: %w", group, err)
	}
	if gid == "" {
		return fmt.Errorf("keycloak group %q not found — apl-core has not provisioned team %q yet", group, team)
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	username := "llz-smoke-" + suffix
	password := "Smoke-" + suffix + "-Aa1!" // satisfy any realm password policy
	clientID := "llz-smoke-" + suffix

	clientUUID, err := k.ensureDirectGrantClient(clientID)
	if err != nil {
		return fmt.Errorf("create smoke client: %w", err)
	}
	defer func() {
		if err := k.deleteClient(clientUUID); err != nil {
			fmt.Fprintf(os.Stderr, "::error::failed to delete smoke client %s: %v — it is a public ROPC client with aud:llz (a standing login path into the OpenBao mount); delete it manually.\n", clientID, err)
			return
		}
		fmt.Fprintf(os.Stderr, "  torn down smoke client %s\n", clientID)
	}()

	uid, err := k.createSmokeUser(username, password)
	if err != nil {
		return fmt.Errorf("create test user: %w", err)
	}
	defer func() {
		// Neutralize before deleting: a disabled orphan can't authenticate even if
		// the delete fails. Surface any teardown failure LOUDLY — a lingering enabled
		// member of the team-<name> group is a standing OpenBao-write credential.
		disabled := k.disableUser(uid) == nil
		if err := k.deleteUser(uid); err != nil {
			state := "DISABLED"
			if !disabled {
				state = "still-ENABLED"
			}
			fmt.Fprintf(os.Stderr, "::error::failed to delete smoke user %s (id %s): %v — a %s member of Keycloak group %s is orphaned in the realm; delete it manually\n", username, uid, err, state, group)
			return
		}
		fmt.Fprintf(os.Stderr, "  torn down test user %s\n", username)
	}()
	if err := k.addUserToGroup(uid, gid); err != nil {
		return fmt.Errorf("add user to %s: %w", group, err)
	}

	// Mint the id_token (browser-free stand-in for the device flow) and prove it
	// carries the apl group→role→claim wiring the OpenBao role binds on.
	idToken, err := k.passwordGrant(clientID, username, password)
	if err != nil {
		return fmt.Errorf("password grant: %w", err)
	}
	groups, err := decodeJWTGroups(idToken)
	if err != nil {
		return fmt.Errorf("decode id_token: %w", err)
	}
	if !containsString(groups, group) {
		return fmt.Errorf("id_token groups %v does not contain %q — apl group→role→claim wiring is broken (OpenBao would 403)", groups, group)
	}
	fmt.Printf("✓ id_token carries groups=%v (contains %s)\n", groups, group)

	// Exchange at OpenBao + assert the scope.
	addr, cleanup, err := portForwardOpenbaoFn()
	if err != nil {
		return fmt.Errorf("port-forward to %s/%s: %w", openbaoNS, rootOpenbaoPod, err)
	}
	defer cleanup()
	ctx := context.Background()
	token, err := openbao.OIDCLogin(ctx, openbao.HTTPClientInsecure(30*time.Second), addr, "keycloak", team, idToken)
	if err != nil {
		return fmt.Errorf("openbao oidc login (mount keycloak, role %s): %w", team, err)
	}
	obc := openbao.NewWithClient(addr, token, "", openbao.HTTPClientInsecure(30*time.Second))

	inPath := subtree + "/_llz_smoke_" + suffix
	if err := obc.Write(ctx, inPath, map[string]string{"ok": "1"}); err != nil {
		return fmt.Errorf("EXPECTED scoped write to %s to SUCCEED, got: %w", inPath, err)
	}
	fmt.Printf("✓ scoped write to %s succeeded\n", inPath)

	outPath := "secret/linode/_llz_smoke_" + suffix
	if err := obc.Write(ctx, outPath, map[string]string{"nope": "1"}); err == nil {
		return fmt.Errorf("SECURITY: out-of-subtree write to %s SUCCEEDED but must be denied — the %s-writer policy is not scoped", outPath, team)
	} else if !isDenied(err) {
		return fmt.Errorf("out-of-subtree write to %s failed, but not with a 403/permission-denied: %w", outPath, err)
	}
	fmt.Printf("✓ out-of-subtree write to %s correctly denied\n", outPath)

	fmt.Printf("SMOKE PASS: team %q — device-identity login → scoped write OK, out-of-subtree denied.\n", team)
	return nil
}

func isDenied(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "403") || strings.Contains(s, "permission denied")
}

// decodeJWTGroups extracts the `groups` claim from a JWT's (unverified) payload.
// Verification is OpenBao's job on the exchange; here we only assert the claim
// shape apl-core is expected to emit.
func decodeJWTGroups(jwt string) ([]string, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("not a JWT (want 3 dot-separated parts)")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	var claims struct {
		Groups []string `json:"groups"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parse claims: %w", err)
	}
	return claims.Groups, nil
}

// ── kcClient smoke helpers (admin REST) ──────────────────────────────────────

// findGroupID returns the realm group id for an EXACT name, or "" if absent.
func (k *kcClient) findGroupID(name string) (string, error) {
	resp, err := k.do(http.MethodGet, "/admin/realms/"+k.realm+"/groups?search="+url.QueryEscape(name), nil)
	if err != nil {
		return "", err
	}
	var groups []struct{ ID, Name string }
	if err := decodeJSON(resp, &groups); err != nil {
		return "", err
	}
	for _, g := range groups {
		if g.Name == name { // search is substring — require exact
			return g.ID, nil
		}
	}
	return "", nil
}

// ensureDirectGrantClient makes a PUBLIC client with direct access grants + the
// openid scope (so its password-grant tokens carry the groups claim). Returns the
// client uuid. Idempotent enough for the smoke (a fresh suffixed id each run).
func (k *kcClient) ensureDirectGrantClient(clientID string) (string, error) {
	body := map[string]any{
		"clientId":                  clientID,
		"protocol":                  "openid-connect",
		"publicClient":              true,
		"standardFlowEnabled":       false,
		"directAccessGrantsEnabled": true,
		"defaultClientScopes":       []string{"openid", "email", "profile"},
	}
	resp, err := k.do(http.MethodPost, "/admin/realms/"+k.realm+"/clients", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create client %s: HTTP %d: %s", clientID, resp.StatusCode, readSnippet(resp.Body))
	}
	uuid := path.Base(resp.Header.Get("Location"))
	if uuid == "" || uuid == "." {
		return "", fmt.Errorf("create client %s: no Location header", clientID)
	}
	// Belt: ensure the openid scope is actually attached (carries the groups claim).
	if err := k.ensureClientDefaultScope(uuid, "openid"); err != nil {
		return uuid, err
	}
	// Stamp `aud: llz` so the smoke token satisfies OpenBao's bound_audiences —
	// this throwaway client mints tokens under its own id, but the role only
	// accepts the llz audience (see keycloakRoleBody.BoundAudiences).
	if err := k.ensureAudienceMapper(uuid, keycloakDeviceClientID); err != nil {
		return uuid, err
	}
	return uuid, nil
}

// createSmokeUser makes an enabled realm user with an inline non-temporary
// password, returning its id.
func (k *kcClient) createSmokeUser(username, password string) (string, error) {
	body := map[string]any{
		"username": username,
		"email":    username + "@llz-smoke.invalid",
		"enabled":  true,
		"credentials": []map[string]any{
			{"type": "password", "value": password, "temporary": false},
		},
	}
	resp, err := k.do(http.MethodPost, "/admin/realms/"+k.realm+"/users", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create user %s: HTTP %d: %s", username, resp.StatusCode, readSnippet(resp.Body))
	}
	uid := path.Base(resp.Header.Get("Location"))
	if uid == "" || uid == "." {
		return "", fmt.Errorf("create user %s: no Location header", username)
	}
	return uid, nil
}

func (k *kcClient) addUserToGroup(userID, groupID string) error {
	resp, err := k.do(http.MethodPut, "/admin/realms/"+k.realm+"/users/"+userID+"/groups/"+groupID, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("add user to group: HTTP %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

func (k *kcClient) deleteUser(userID string) error {
	resp, err := k.do(http.MethodDelete, "/admin/realms/"+k.realm+"/users/"+userID, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Previously the status was ignored, so a failed delete looked like success and
	// left a real team-member user standing. Surface non-success (404 = already gone).
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete user %s: HTTP %d: %s", userID, resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

// disableUser sets enabled:false — a belt on smoke teardown so an orphan that
// can't be deleted at least can't authenticate as a team member.
func (k *kcClient) disableUser(userID string) error {
	resp, err := k.do(http.MethodPut, "/admin/realms/"+k.realm+"/users/"+userID, map[string]any{"enabled": false})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("disable user %s: HTTP %d: %s", userID, resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

func (k *kcClient) deleteClient(clientUUID string) error {
	resp, err := k.do(http.MethodDelete, "/admin/realms/"+k.realm+"/clients/"+clientUUID, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Check the status like deleteUser: a leaked smoke client is a PUBLIC,
	// ROPC-enabled client stamped with aud:llz — a standing password-grant login
	// path into the OpenBao mount — so a silently-failed delete must not look clean.
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete client %s: HTTP %d: %s", clientUUID, resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

// passwordGrant runs the OAuth2 Resource-Owner-Password-Credentials grant against
// the realm (public client, scope openid), returning the id_token.
func (k *kcClient) passwordGrant(clientID, username, password string) (string, error) {
	form := url.Values{
		"grant_type": {"password"}, "client_id": {clientID},
		"username": {username}, "password": {password}, "scope": {"openid"},
	}
	resp, err := k.hc.PostForm(k.base+"/realms/"+k.realm+"/protocol/openid-connect/token", form)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("password grant: HTTP %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	var out struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	if out.IDToken == "" {
		return "", fmt.Errorf("password grant returned no id_token (is the openid scope attached + a groups mapper present?)")
	}
	return out.IDToken, nil
}
