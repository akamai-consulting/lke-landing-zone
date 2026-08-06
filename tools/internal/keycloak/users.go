package keycloak

import (
	"fmt"
	"net/http"
	"net/url"
	"path"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/apl/identity"
)

// users.go — the user/role operations `llz users add` drives, moved here for the
// same language reason as provision.go: they are methods on Client.
//
// THIRD CALLER, and the pattern is now unmistakable. Every part of this repo that
// talks to Keycloak extends the client rather than wrapping it, so the client
// cannot live in any one of them. That is not a design decision anyone made — it
// is Go's method rules deciding where the boundary goes.

func (k *Client) FindRealmRole(name string) (*identity.Role, error) {
	resp, err := k.Do(http.MethodGet, "/admin/realms/"+k.Realm+"/roles/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	var role identity.Role
	if err := decodeJSON(resp, &role); err != nil {
		return nil, err
	}
	if role.ID == "" {
		return nil, nil
	}
	return &role, nil
}
func (k *Client) EnsureUser(rep identity.UserRep) (string, bool, error) {
	resp, err := k.Do(http.MethodPost, "/admin/realms/"+k.Realm+"/users", rep)
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
		uid, ferr := k.FindUserByUsername(rep.Username)
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
func (k *Client) FindUserByUsername(username string) (string, error) {
	resp, err := k.Do(http.MethodGet, "/admin/realms/"+k.Realm+"/users?exact=true&username="+url.QueryEscape(username), nil)
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
func (k *Client) AssignRealmRoles(userID string, roles []identity.Role) error {
	if len(roles) == 0 {
		return nil
	}
	resp, err := k.Do(http.MethodPost, "/admin/realms/"+k.Realm+"/users/"+userID+"/role-mappings/realm", roles)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("assign realm roles: HTTP %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}
func (k *Client) SendUpdatePasswordEmail(userID string) error {
	resp, err := k.Do(http.MethodPut, "/admin/realms/"+k.Realm+"/users/"+userID+"/execute-actions-email", []string{"UPDATE_PASSWORD"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("execute-actions-email: HTTP %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}
