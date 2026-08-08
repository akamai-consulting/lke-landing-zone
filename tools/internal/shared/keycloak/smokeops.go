package keycloak

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
)

// smokeops.go — the admin-REST operations the identity SMOKE lane needs, which
// package main's `llz users add` also calls.
//
// THE THIRD PLACE THIS PATTERN APPEARED IN ONE EXTRACTION. These began as methods
// on the smoke lane's local wrapper, which worked until users.go turned out to
// call findGroupID and addUserToGroup too. Every operation on this client ends up
// here eventually — the client is the shared thing, and an operation ON it cannot
// live anywhere else once a second caller appears.
//
// ensureDirectGrantClient and deleteClient MUTATE Keycloak: the smoke lane creates
// a temporary direct-grant client to drive a login and removes it afterward. That
// is why assert-identity declares a transition binding rather than being a pure
// assertion — see its extension.go.

func (k *Client) RealmRoleExists(name string) (bool, error) {
	resp, err := k.Do(http.MethodGet, "/admin/realms/"+k.Realm+"/roles/"+url.PathEscape(name), nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("look up realm role %s: HTTP %d: %s", name, resp.StatusCode, readSnippet(resp.Body))
	}
}
func (k *Client) FindGroupID(name string) (string, error) {
	resp, err := k.Do(http.MethodGet, "/admin/realms/"+k.Realm+"/groups?search="+url.QueryEscape(name), nil)
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
func (k *Client) EnsureDirectGrantClient(clientID string) (string, error) {
	body := map[string]any{
		"clientId":                  clientID,
		"protocol":                  "openid-connect",
		"publicClient":              true,
		"standardFlowEnabled":       false,
		"directAccessGrantsEnabled": true,
		"defaultClientScopes":       []string{"openid", "email", "profile"},
	}
	resp, err := k.Do(http.MethodPost, "/admin/realms/"+k.Realm+"/clients", body)
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
	if err := k.EnsureClientDefaultScope(uuid, "openid"); err != nil {
		return uuid, err
	}
	// Stamp `aud: llz` so the smoke token satisfies OpenBao's bound_audiences —
	// this throwaway client mints tokens under its own id, but the role only
	// accepts the llz audience (see keycloakRoleBody.BoundAudiences).
	if err := k.EnsureAudienceMapper(uuid, DeviceClientID); err != nil {
		return uuid, err
	}
	return uuid, nil
}
func (k *Client) CreateSmokeUser(username, password string) (string, error) {
	// Fully set the user up so the direct-grant login isn't blocked by the realm's
	// DEFAULT required actions (VERIFY_EMAIL / UPDATE_PASSWORD / UPDATE_PROFILE /
	// CONFIGURE_TOTP) — those surface as "Account is not fully set up" at token time.
	// emailVerified + a profile (names) satisfy the profile/email checks; an explicit
	// empty requiredActions overrides the realm defaults; the credential is permanent.
	body := map[string]any{
		"username":        username,
		"email":           username + "@llz-smoke.invalid",
		"emailVerified":   true,
		"firstName":       "LLZ",
		"lastName":        "Smoke",
		"enabled":         true,
		"requiredActions": []string{},
		"credentials": []map[string]any{
			{"type": "password", "value": password, "temporary": false},
		},
	}
	resp, err := k.Do(http.MethodPost, "/admin/realms/"+k.Realm+"/users", body)
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
func (k *Client) AddUserToGroup(userID, groupID string) error {
	resp, err := k.Do(http.MethodPut, "/admin/realms/"+k.Realm+"/users/"+userID+"/groups/"+groupID, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("add user to group: HTTP %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}
func (k *Client) AddRealmRoleToUser(userID, roleName string) error {
	// The role representation (id + name) POST body role-mappings/realm requires.
	resp, err := k.Do(http.MethodGet, "/admin/realms/"+k.Realm+"/roles/"+url.PathEscape(roleName), nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return fmt.Errorf("realm role %q not found — apl-core has not provisioned the team role yet", roleName)
	}
	var role struct{ ID, Name string }
	if err := decodeJSON(resp, &role); err != nil {
		return err
	}
	pr, err := k.Do(http.MethodPost, "/admin/realms/"+k.Realm+"/users/"+userID+"/role-mappings/realm",
		[]map[string]string{{"id": role.ID, "name": role.Name}})
	if err != nil {
		return err
	}
	defer pr.Body.Close()
	if pr.StatusCode != http.StatusNoContent && pr.StatusCode != http.StatusOK {
		return fmt.Errorf("assign realm role %s: HTTP %d: %s", roleName, pr.StatusCode, readSnippet(pr.Body))
	}
	return nil
}
func (k *Client) DeleteUser(userID string) error {
	resp, err := k.Do(http.MethodDelete, "/admin/realms/"+k.Realm+"/users/"+userID, nil)
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
func (k *Client) DisableUser(userID string) error {
	resp, err := k.Do(http.MethodPut, "/admin/realms/"+k.Realm+"/users/"+userID, map[string]any{"enabled": false})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("disable user %s: HTTP %d: %s", userID, resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}
func (k *Client) DeleteClient(clientUUID string) error {
	resp, err := k.Do(http.MethodDelete, "/admin/realms/"+k.Realm+"/clients/"+clientUUID, nil)
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
func (k *Client) PasswordGrant(clientID, username, password string) (string, error) {
	form := url.Values{
		"grant_type": {"password"}, "client_id": {clientID},
		"username": {username}, "password": {password}, "scope": {"openid"},
	}
	resp, err := k.HC.PostForm(k.Base+"/realms/"+k.Realm+"/protocol/openid-connect/token", form)
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
