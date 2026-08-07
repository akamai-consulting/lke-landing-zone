package keycloak

// Package keycloak is the thin Keycloak admin-REST client the provisioner and the
// identity assertion share.
//
// IT IS A PACKAGE BECAUSE OF A GO CONSTRAINT, not a caller count. The team-login
// smoke lane defines METHODS on this type — realmRoleExists, findGroupID,
// ensureDirectGrantClient — and Go will not let a package add methods to a type it
// does not own. So `assert-identity` could not be extracted at all while the
// client lived in package main: the choice was to move the client out, or to move
// the provisioner in and merge two extensions the branch has already decided
// should stay separate.
//
// That is a new reason for a shared package. The previous twelve came out on
// caller count (guardwalk at ten, cigate at twelve) or to break a dependency cycle
// (instancelayout). This one came out because the LANGUAGE required it, which is
// worth noticing: an extraction boundary can be forced by Go's method rules quite
// apart from what the design wants.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a thin Keycloak admin-REST client bound to one realm.
type Client struct {
	HC    *http.Client
	Base  string
	Token string
	Realm string
}

func (k *Client) Do(method, apiPath string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, k.Base+apiPath, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+k.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return k.HC.Do(req)
}

// Constants the moved provisioning methods need. Copies rather than imports:
// package main still owns the configure verb that also reads them, and a const is
// not a seam.
const (
	// NS / AdminSecret / Realm are the platform's Keycloak coordinates. They live
	// with the client because every caller that talks to Keycloak needs the same
	// three, and a per-caller copy is how a namespace rename becomes a silent
	// half-migration.
	NS          = "keycloak"
	AdminSecret = "keycloak-admin-credentials"
	Realm       = "otomi"

	// PlatformAdminRole is apl-core's built-in all-teams platform-admin realm role.
	// A COPY of package main's const: the openbao configure verb reads it too, and
	// a realm-role NAME is a fact rather than a seam.
	PlatformAdminRole = "platform-admin"

	// DeviceClientID is the public client the device-code login flow uses.
	DeviceClientID = "llz"
)

// decodeJSON reads a JSON array/object body, requiring a 2xx status.
func decodeJSON(resp *http.Response, v any) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return strings.TrimSpace(string(b))
}

// AdminToken exchanges the admin username/password for an access token
// via the master realm's direct-grant flow (client admin-cli).
func AdminToken(hc *http.Client, base, user, pass string) (string, error) {
	form := url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"},
		"username": {user}, "password": {pass},
	}
	resp, err := hc.PostForm(base+"/realms/master/protocol/openid-connect/token", form)
	if err != nil {
		return "", fmt.Errorf("keycloak admin token: %w", err)
	}
	defer resp.Body.Close()
	// 401/403 is a PERMANENT auth failure (wrong/disabled admin) — flag it so the
	// readiness retry fails fast instead of masking it as a not-ready timeout. Other
	// non-200s (5xx, 404 during realm import) are treated as transient (retryable).
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("%w: HTTP %d: %s", ErrAuthDenied, resp.StatusCode, readSnippet(resp.Body))
	}
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

// keycloakConnect port-forwards to Keycloak and master-realm direct-grants an
// admin token, retrying the pair until it succeeds or the ~5m keycloakScope budget
// expires. keycloak-configure runs early in bootstrap, ahead of apl-core bringing
// Keycloak up, so a single attempt would skip on a not-yet-ready server — a
// successful admin token is the readiness signal. Returns the live port-forward's
// base URL + admin token + its cleanup (a no-op cleanup on failure). Best-effort:
// the caller warns + exits 0 on a persistent failure.
// ErrAuthDenied marks a PERMANENT admin-token failure (401/403: wrong or
// disabled master admin) so keycloakConnect fails fast rather than retrying it as
// a transient not-ready condition (which would burn the ~5m budget and misreport
// a credential problem as a readiness timeout).
var ErrAuthDenied = errors.New("keycloak admin auth denied")

// ScopeAttempts / ScopeInterval bound the wait for a client scope to appear —
// Keycloak creates it asynchronously after the client POST returns.
//
// VARS, NOT CONSTS, AND EXPORTED. They were consts in package main and its
// mutation test tuned them down to keep the poll-budget assertion fast. Moving
// them here as consts silently broke that test: it still set package main's
// copies, which nothing read any more, so the real loop polled 30 times against
// an expectation of 4. A bound a test needs to tune is a seam, and a seam that
// crosses a package boundary has to be exported to stay one.
var (
	ScopeAttempts = 30
	ScopeInterval = 10 * time.Second
)
