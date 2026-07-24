package main

// ci_keycloak_configure.go — `llz ci keycloak-configure`, the Keycloak-realm half
// of the team-scoped-credentials turnkey path. `llz ci bao-configure` provisions
// the OpenBao side (the keycloak auth mount + per-team policy/role); this ensures
// the matching realm objects so `llz openbao login` works with no manual console
// steps: a device-flow OIDC client, a `groups` claim mapper on it, and one realm
// group per spec.teams entry.
//
// It reaches Keycloak over an ephemeral kubectl port-forward to the keycloak pod
// (the pods/portforward subresource is allowed on LKE-E even where the apiserver
// service-proxy is webhook-denied), authenticates to the master realm with the
// in-cluster admin creds, and drives the admin REST API. Every interaction is
// best-effort: any failure WARNS and falls back to the manual runbook step
// (docs/runbooks/openbao-team-login.md) rather than failing — so an unexpected
// Keycloak API shape can never wedge the bootstrap it runs inside. See ADR 0004.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"time"

	"github.com/spf13/cobra"
)

const (
	keycloakNS       = "keycloak"
	keycloakPod      = "keycloak-keycloakx-0" // apl-core's Keycloak.X StatefulSet pod-0
	keycloakHTTPPort = "8080"                 // in-pod plaintext listener (TLS terminates at the edge)
	keycloakRealm    = "otomi"
	// keycloakDeviceClientID is the public OIDC client `llz openbao login` uses;
	// overridable there via --client-id / OPENBAO_OIDC_CLIENT_ID.
	keycloakDeviceClientID = "llz"
	// keycloakAdminSecret holds the MASTER-realm admin creds (keycloakAdminToken
	// direct-grants against /realms/master with client admin-cli). On managed
	// apl-core that is `keycloak-initial-admin` — the secret the Keycloak.X
	// StatefulSet consumes as KC_BOOTSTRAP_ADMIN_USERNAME/PASSWORD. The old
	// `platform-admin-credentials` was a self-installed-era name that NOTHING
	// provisions on managed, so keycloak-configure read empty creds and
	// warnKeycloakSkip'd every run — leaving the device-flow client uncreated and
	// team-OIDC OpenBao login silently unavailable. (The otomi-realm portal login
	// `platform-admin-initial-credentials` is a DIFFERENT secret and cannot
	// master-realm direct-grant.)
	keycloakAdminSecret = "keycloak-initial-admin"
)

// Bootstrap ordering guard: how long keycloak-configure waits for apl-core to
// converge the `openid` client scope before wiring the device client. ~5 min
// (30 × 10s) — apl-core Keycloak is usually up well before then. Vars (not
// consts) so tests can shrink them.
var (
	keycloakScopeAttempts = 30
	keycloakScopeInterval = 10 * time.Second
	keycloakSleepFn       = time.Sleep
)

// portForwardKeycloakFn opens a port-forward to the Keycloak pod's HTTP port and
// returns the local base URL + teardown. A package var so tests seam it.
var portForwardKeycloakFn = portForwardKeycloak

func portForwardKeycloak() (string, func(), error) {
	cmd := exec.Command("kubectl", "port-forward", "-n", keycloakNS, "pod/"+keycloakPod, ":"+keycloakHTTPPort)
	cmd.Stderr = os.Stderr // surface wrong-context / pod-absent / RBAC errors (see portForwardOpenbao)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("kubectl port-forward: %w", err)
	}
	stop := func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }
	localPort, err := readForwardPortTimeout(stdout, forwardEstablishTimeout)
	if err != nil {
		stop()
		return "", nil, err
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	return "http://127.0.0.1:" + localPort, stop, nil
}

// kcClient is a thin Keycloak admin-REST client bound to one realm.
type kcClient struct {
	hc    *http.Client
	base  string
	token string
	realm string
}

func (k *kcClient) do(method, apiPath string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, k.base+apiPath, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return k.hc.Do(req)
}

// keycloakAdminToken exchanges the admin username/password for an access token
// via the master realm's direct-grant flow (client admin-cli).
func keycloakAdminToken(hc *http.Client, base, user, pass string) (string, error) {
	form := url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"},
		"username": {user}, "password": {pass},
	}
	resp, err := hc.PostForm(base+"/realms/master/protocol/openid-connect/token", form)
	if err != nil {
		return "", fmt.Errorf("keycloak admin token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("keycloak admin token: HTTP %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("parse keycloak admin token: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("keycloak admin token response had no access_token")
	}
	return out.AccessToken, nil
}

// ensureDeviceClient makes the public device-flow client exist and returns its
// UUID (idempotent: returns the existing client's id when already present).
func (k *kcClient) ensureDeviceClient(clientID string) (string, error) {
	uuid, err := k.getOrCreateClient(clientID)
	if err != nil {
		return "", err
	}
	// Reconcile the `openid` default scope, which carries apl-core's `groups`
	// claim. Do this even for a PRE-EXISTING client: `defaultClientScopes` in the
	// create body is honored only if the scope existed at create time, so a client
	// created before apl-core converged its `openid` scope would otherwise be
	// stuck without the groups claim and `llz openbao login` would 403 forever.
	if err := k.ensureClientDefaultScope(uuid, "openid"); err != nil {
		return uuid, err
	}
	// Stamp `aud: llz` so the OpenBao keycloak role's bound_audiences accepts this
	// client's tokens (and rejects arbitrary other-client realm tokens).
	if err := k.ensureAudienceMapper(uuid, keycloakDeviceClientID); err != nil {
		return uuid, err
	}
	return uuid, nil
}

// ensureAudienceMapper adds an OIDC audience protocol mapper that stamps
// `aud: <audience>` into the client's id + access tokens, so OpenBao's keycloak
// role (bound_audiences=[llz]) accepts tokens this client mints and rejects
// tokens minted for any other realm client. Idempotent: a 409 (mapper already
// present) is success.
func (k *kcClient) ensureAudienceMapper(clientUUID, audience string) error {
	body := map[string]any{
		"name":           "llz-openbao-audience",
		"protocol":       "openid-connect",
		"protocolMapper": "oidc-audience-mapper",
		"config": map[string]string{
			"included.client.audience": audience,
			"id.token.claim":           "true",
			"access.token.claim":       "true",
		},
	}
	resp, err := k.do(http.MethodPost, "/admin/realms/"+k.realm+"/clients/"+clientUUID+"/protocol-mappers/models", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("add audience mapper to client %s: HTTP %d: %s", clientUUID, resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

// getOrCreateClient returns the UUID of clientID, creating the public device-flow
// client (idempotent) when it doesn't exist yet.
func (k *kcClient) getOrCreateClient(clientID string) (string, error) {
	resp, err := k.do(http.MethodGet, "/admin/realms/"+k.realm+"/clients?clientId="+url.QueryEscape(clientID), nil)
	if err != nil {
		return "", err
	}
	var existing []struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(resp, &existing); err != nil {
		return "", err
	}
	if len(existing) > 0 && existing[0].ID != "" {
		return existing[0].ID, nil
	}
	body := map[string]any{
		"clientId":                  clientID,
		"protocol":                  "openid-connect",
		"publicClient":              true,
		"standardFlowEnabled":       false,
		"directAccessGrantsEnabled": false,
		"attributes":                map[string]string{"oauth2.device.authorization.grant.enabled": "true"},
		// Best-effort at create time; ensureClientDefaultScope is the authority
		// (it also fixes a client created before apl-core's `openid` scope existed).
		"defaultClientScopes": []string{"openid", "email", "profile"},
	}
	cresp, err := k.do(http.MethodPost, "/admin/realms/"+k.realm+"/clients", body)
	if err != nil {
		return "", err
	}
	defer cresp.Body.Close()
	if cresp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create client %s: HTTP %d: %s", clientID, cresp.StatusCode, readSnippet(cresp.Body))
	}
	// Keycloak returns the new resource in the Location header.
	if loc := cresp.Header.Get("Location"); loc != "" {
		return path.Base(loc), nil
	}
	// Fallback: re-query (won't recurse past the create — the client now exists).
	return k.getOrCreateClient(clientID)
}

// ensureClientDefaultScope attaches realm client scope `name` to the client as a
// DEFAULT scope if not already assigned (idempotent). Returns an actionable error
// when the realm scope doesn't exist yet (apl-core hasn't converged Keycloak) —
// the caller warns and the bootstrap re-run fixes it once the scope appears.
func (k *kcClient) ensureClientDefaultScope(clientUUID, name string) error {
	base := "/admin/realms/" + k.realm + "/clients/" + clientUUID + "/default-client-scopes"
	resp, err := k.do(http.MethodGet, base, nil)
	if err != nil {
		return err
	}
	var assigned []struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(resp, &assigned); err != nil {
		return err
	}
	for _, s := range assigned {
		if s.Name == name {
			return nil // already a default scope
		}
	}
	scopeID, err := k.findClientScopeID(name)
	if err != nil {
		return err
	}
	if scopeID == "" {
		return fmt.Errorf("realm client scope %q not found — apl-core may not have converged Keycloak yet; the device client lacks the groups claim until it exists (re-run once apl-core is up)", name)
	}
	presp, err := k.do(http.MethodPut, base+"/"+scopeID, nil)
	if err != nil {
		return err
	}
	defer presp.Body.Close()
	if presp.StatusCode != http.StatusNoContent && presp.StatusCode != http.StatusOK {
		return fmt.Errorf("assign default scope %s: HTTP %d: %s", name, presp.StatusCode, readSnippet(presp.Body))
	}
	return nil
}

// findClientScopeID returns the realm client-scope id for name, or "" if the
// realm has no such scope yet.
func (k *kcClient) findClientScopeID(name string) (string, error) {
	resp, err := k.do(http.MethodGet, "/admin/realms/"+k.realm+"/client-scopes", nil)
	if err != nil {
		return "", err
	}
	var scopes []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := decodeJSON(resp, &scopes); err != nil {
		return "", err
	}
	for _, s := range scopes {
		if s.Name == name {
			return s.ID, nil
		}
	}
	return "", nil
}

// waitForClientScope blocks until the realm client scope `name` exists (apl-core
// provisions it asynchronously), polling keycloakScopeAttempts times with
// keycloakScopeInterval between tries. It guards the bootstrap ordering race: we
// must not wire the device client before its groups-carrying scope exists, or the
// client is created scope-less and `llz openbao login` 403s. Returns an
// actionable error if the scope never appears (the caller warns, best-effort).
func (k *kcClient) waitForClientScope(name string, sleep func(time.Duration)) error {
	for i := 0; i < keycloakScopeAttempts; i++ {
		id, err := k.findClientScopeID(name)
		if err != nil {
			return err
		}
		if id != "" {
			return nil
		}
		if i < keycloakScopeAttempts-1 {
			sleep(keycloakScopeInterval)
		}
	}
	return fmt.Errorf("realm client scope %q did not appear after ~%s — apl-core Keycloak has not converged it; re-run `llz ci keycloak-configure` once apl-core is up", name, time.Duration(keycloakScopeAttempts)*keycloakScopeInterval)
}

// NOTE: we deliberately do NOT create realm groups or a `groups` claim mapper.
// apl-core owns both: it provisions a `team-<name>` group + realm role from the
// teamConfig `llz render` emits, and its default `openid` client scope already
// carries a `groups` realm-role claim. This command's only job is the one thing
// apl-core won't do — a PUBLIC device-flow client for `llz openbao login`.

// decodeJSON reads a JSON array/object body, requiring a 2xx status.
func decodeJSON(resp *http.Response, v any) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func ciKeycloakConfigureCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "keycloak-configure",
		Short: "ensure the Keycloak device-flow client for team-scoped OpenBao login (spec.teams)",
		Long: "Realm half of the team-scoped-credentials turnkey path (bao-configure owns\n" +
			"the OpenBao half). Port-forwards to the Keycloak pod, authenticates with the\n" +
			"in-cluster " + keycloakAdminSecret + " admin creds, then idempotently ensures a\n" +
			"single PUBLIC device-flow OIDC client (" + keycloakDeviceClientID + ") carrying the default `openid`\n" +
			"scope. The per-team groups + `groups` claim are apl-core's job (the native\n" +
			"team-<name> group/role from the teamConfig `llz render` emits), so this does\n" +
			"NOT create groups or mappers. Best-effort: any Keycloak failure WARNS (and\n" +
			"points at the manual runbook step) rather than failing, so it is safe in the\n" +
			"bootstrap path. No-op when spec.teams is empty.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIKeycloakConfigure(gopts, region) },
	}
	c.Flags().StringVar(&region, "region", "", "region name used in operator-facing messages (required)")
	return c
}

func runCIKeycloakConfigure(g globalOpts, region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	// spec.teams is the intent gate: no teams → no team login path → no client.
	if teams := specTeams(); len(teams) == 0 {
		fmt.Println("No spec.teams declared — nothing to configure in Keycloak.")
		return nil
	}
	if g.dryRun {
		fmt.Fprintf(os.Stderr, "→ (dry-run) would ensure the public device-flow client %q (openid scope) in realm %s.\n", keycloakDeviceClientID, keycloakRealm)
		return nil
	}

	// Best-effort from here: warn + succeed on any Keycloak-side failure so a
	// realm/API-shape surprise never wedges the bootstrap this runs in. The
	// manual fallback is docs/runbooks/openbao-team-login.md step 3.
	user := k8sSecretField(keycloakNS, keycloakAdminSecret, "username")
	pass := k8sSecretField(keycloakNS, keycloakAdminSecret, "password")
	if user == "" || pass == "" {
		warnKeycloakSkip(region, fmt.Errorf("admin creds not readable from %s/%s (keys username/password)", keycloakNS, keycloakAdminSecret))
		return nil
	}

	base, cleanup, err := portForwardKeycloakFn()
	if err != nil {
		warnKeycloakSkip(region, fmt.Errorf("port-forward to %s/%s: %w", keycloakNS, keycloakPod, err))
		return nil
	}
	defer cleanup()

	hc := &http.Client{Timeout: 20 * time.Second}
	token, err := keycloakAdminToken(hc, base, user, pass)
	if err != nil {
		warnKeycloakSkip(region, err)
		return nil
	}
	k := &kcClient{hc: hc, base: base, token: token, realm: keycloakRealm}

	// Ordering guard: wait for apl-core to converge the `openid` client scope
	// (which carries the groups claim) BEFORE wiring the client, so a bootstrap
	// that runs ahead of apl-core doesn't create a scope-less client that 403s at
	// login. Best-effort — if it never appears, warn + exit 0 (the re-run fixes it).
	if err := k.waitForClientScope("openid", keycloakSleepFn); err != nil {
		warnKeycloakSkip(region, err)
		return nil
	}

	if _, err := k.ensureDeviceClient(keycloakDeviceClientID); err != nil {
		warnKeycloakSkip(region, fmt.Errorf("ensure device client %s: %w", keycloakDeviceClientID, err))
		return nil
	}
	fmt.Printf("Keycloak client %q ready (public device flow, openid scope) — operators can `llz openbao login --team <name>`.\n", keycloakDeviceClientID)
	return nil
}

func warnKeycloakSkip(region string, err error) {
	fmt.Fprintf(os.Stderr, "::warning::keycloak-configure on %s could not finish (%v) — team OIDC login stays unavailable until the realm is wired by hand (docs/runbooks/openbao-team-login.md step 3). This does not block the bootstrap.\n", region, err)
}
