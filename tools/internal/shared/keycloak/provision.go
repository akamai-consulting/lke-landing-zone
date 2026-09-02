package keycloak

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"time"
)

// provision.go — the admin-REST operations, moved here with the Client.
//
// THEY HAD TO COME. Go will not let a package define methods on a type it does not
// own, and BOTH sides of the keycloak-provisioner ↔ assert-identity pair extend
// this client: the provisioner with getOrCreateClient and friends, the smoke lane
// with realmRoleExists and findGroupID. Once the type moved out of package main,
// every method on it had to follow.
//
// That is the constraint doing the design rather than the design choosing: these
// operations would sit comfortably in the provisioner extension, and the language
// puts them here instead. Worth recording as the cost of the split — the alternative
// was merging two extensions the branch has already decided should stay separate.

func (k *Client) EnsureDeviceClient(clientID string) (string, error) {
	uuid, err := k.GetOrCreateClient(clientID)
	if err != nil {
		return "", err
	}
	// Reconcile the `openid` default scope, which carries apl-core's `groups`
	// claim. Do this even for a PRE-EXISTING client: `defaultClientScopes` in the
	// create body is honored only if the scope existed at create time, so a client
	// created before apl-core converged its `openid` scope would otherwise be
	// stuck without the groups claim and `llz openbao login` would 403 forever.
	if err := k.EnsureClientDefaultScope(uuid, "openid"); err != nil {
		return uuid, err
	}
	// Backfill `name` on a client created before it was set. Same reasoning as the
	// create body, but this is the half that matters for an ALREADY-BOOTSTRAPPED
	// cluster: those clients are nameless today and are actively deadlocking
	// apl-core's realm reconcile, so re-running `llz ci keycloak-configure` has to
	// be the repair path rather than a no-op.
	if err := k.EnsureClientName(uuid, clientID); err != nil {
		return uuid, err
	}
	// Stamp `aud: llz` so the OpenBao keycloak role's bound_audiences accepts this
	// client's tokens (and rejects arbitrary other-client realm tokens).
	if err := k.EnsureAudienceMapper(uuid, DeviceClientID); err != nil {
		return uuid, err
	}
	return uuid, nil
}

// EnsureClientName gives the client a non-empty `name`, leaving an existing one
// alone. See GetOrCreateClient for why a nameless client breaks apl-core.
func (k *Client) EnsureClientName(clientUUID, name string) error {
	base := "/admin/realms/" + k.Realm + "/clients/" + clientUUID
	resp, err := k.Do(http.MethodGet, base, nil)
	if err != nil {
		return err
	}
	var rep map[string]any
	if err := decodeJSON(resp, &rep); err != nil {
		return err
	}
	if existing, _ := rep["name"].(string); existing != "" {
		return nil // already named — never rename a client someone set deliberately
	}
	// PUT the full representation back rather than a bare {"name": …}: Keycloak
	// merges absent fields, but round-tripping what it just gave us keeps this
	// honest if that ever changes.
	rep["name"] = name
	presp, err := k.Do(http.MethodPut, base, rep)
	if err != nil {
		return err
	}
	defer presp.Body.Close()
	if presp.StatusCode != http.StatusNoContent && presp.StatusCode != http.StatusOK {
		return fmt.Errorf("set name on client %s: HTTP %d: %s", clientUUID, presp.StatusCode, readSnippet(presp.Body))
	}
	return nil
}
func (k *Client) EnsureAudienceMapper(clientUUID, audience string) error {
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
	resp, err := k.Do(http.MethodPost, "/admin/realms/"+k.Realm+"/clients/"+clientUUID+"/protocol-mappers/models", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("add audience mapper to client %s: HTTP %d: %s", clientUUID, resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}
func (k *Client) GetOrCreateClient(clientID string) (string, error) {
	resp, err := k.Do(http.MethodGet, "/admin/realms/"+k.Realm+"/clients?clientId="+url.QueryEscape(clientID), nil)
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
		"clientId": clientID,
		// A NAME IS NOT COSMETIC HERE. apl-core's keycloak operator locates the
		// `otomi` client it reconciles with `allClients.find(el => el.name ===
		// client.name)`, and its own template never sets `name` — so that lookup
		// means "the first client with no name". A nameless client of ours sorts
		// before `otomi` and silently steals the match: the operator then PUTs the
		// otomi representation (authorizationServicesEnabled: true) onto THIS
		// public client, Keycloak 500s with "Only confidential clients are allowed
		// to set authorization settings", and the whole realm reconcile — realm,
		// roles, IDP, and every APL console user after it — halts on a 30s retry
		// loop. Symptom is console logins failing `user_not_found` for users that
		// exist in `apl-users`. Always give the client a name.
		"name":                      clientID,
		"protocol":                  "openid-connect",
		"publicClient":              true,
		"standardFlowEnabled":       false,
		"directAccessGrantsEnabled": false,
		"attributes":                map[string]string{"oauth2.device.authorization.grant.enabled": "true"},
		// Best-effort at create time; ensureClientDefaultScope is the authority
		// (it also fixes a client created before apl-core's `openid` scope existed).
		"defaultClientScopes": []string{"openid", "email", "profile"},
	}
	cresp, err := k.Do(http.MethodPost, "/admin/realms/"+k.Realm+"/clients", body)
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
	return k.GetOrCreateClient(clientID)
}
func (k *Client) EnsureClientDefaultScope(clientUUID, name string) error {
	base := "/admin/realms/" + k.Realm + "/clients/" + clientUUID + "/default-client-scopes"
	resp, err := k.Do(http.MethodGet, base, nil)
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
	scopeID, err := k.FindClientScopeID(name)
	if err != nil {
		return err
	}
	if scopeID == "" {
		return fmt.Errorf("realm client scope %q not found — apl-core may not have converged Keycloak yet; the device client lacks the groups claim until it exists (re-run once apl-core is up)", name)
	}
	presp, err := k.Do(http.MethodPut, base+"/"+scopeID, nil)
	if err != nil {
		return err
	}
	defer presp.Body.Close()
	if presp.StatusCode != http.StatusNoContent && presp.StatusCode != http.StatusOK {
		return fmt.Errorf("assign default scope %s: HTTP %d: %s", name, presp.StatusCode, readSnippet(presp.Body))
	}
	return nil
}
func (k *Client) FindClientScopeID(name string) (string, error) {
	resp, err := k.Do(http.MethodGet, "/admin/realms/"+k.Realm+"/client-scopes", nil)
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
func (k *Client) WaitForClientScope(name string, sleep func(time.Duration)) error {
	for i := 0; i < ScopeAttempts; i++ {
		id, err := k.FindClientScopeID(name)
		if err != nil {
			return err
		}
		if id != "" {
			return nil
		}
		if i < ScopeAttempts-1 {
			sleep(ScopeInterval)
		}
	}
	return fmt.Errorf("realm client scope %q did not appear after ~%s — apl-core Keycloak has not converged it; re-run `llz ci keycloak-configure` once apl-core is up", name, time.Duration(ScopeAttempts)*ScopeInterval)
}
