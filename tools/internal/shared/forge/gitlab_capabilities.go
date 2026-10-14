package forge

// gitlab_capabilities.go — the GitLab network capabilities: SecretWriter
// (CI/CD variables), TokenMinter + TokenRotator (project access tokens, incl.
// self_rotate — the reason TokenRotator exists as a GitLab-only interface), and
// ExpiryProber. This is where the design's payoff lives: a project access token
// with self_rotate renews its own lifetime, so a GitLab instance carries zero
// permanent service secrets, unlike the GitHub family's App private key.
//
// UNVALIDATED AGAINST A LIVE GITLAB. Endpoint shapes follow the documented v4
// REST API; the unit tests exercise this code's URL/param/parse logic against an
// httptest server, not a real instance. End-to-end validation is gated on a
// GitLab project in the e2e harness (docs/designs/forge-abstraction.md, Phase 6),
// which does not exist yet — forge.Supported still rejects a GitLab spec.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitLabClient implements SecretWriter, TokenMinter, TokenRotator and
// ExpiryProber for one project. Construct it with NewGitLabClient.
type GitLabClient struct {
	apiBase string // https://<host>/api/v4
	token   string
	project string // "group/project" or numeric id
	client  *http.Client
	now     func() time.Time // injectable clock for expires_at (default time.Now)
}

var (
	_ SecretWriter = (*GitLabClient)(nil)
	_ TokenMinter  = (*GitLabClient)(nil)
	_ TokenRotator = (*GitLabClient)(nil)
	_ ExpiryProber = (*GitLabClient)(nil)
)

// NewGitLabClient builds a client against a GitLab forge, authenticating as
// token, targeting the group/project (or numeric id) project.
func NewGitLabClient(f Forge, token, project string) (*GitLabClient, error) {
	if f.Flavor() != GitLab {
		return nil, fmt.Errorf("NewGitLabClient: forge %q is not GitLab", f.Flavor())
	}
	if token == "" || project == "" {
		return nil, fmt.Errorf("GitLab client needs a token and a group/project")
	}
	return &GitLabClient{
		apiBase: f.APIBase(),
		token:   token,
		project: project,
		client:  &http.Client{Timeout: 15 * time.Second},
		now:     time.Now,
	}, nil
}

// projectPath is the URL-encoded project id GitLab's /projects/:id endpoints
// take ("group/project" → "group%2Fproject").
func (c *GitLabClient) projectPath() string { return url.PathEscape(c.project) }

func (c *GitLabClient) auth(req *http.Request) {
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Accept", "application/json")
}

// --- SecretWriter: CI/CD variables ---

// SetRepoSecret writes a masked, project-wide CI/CD variable.
func (c *GitLabClient) SetRepoSecret(name, value string) error {
	return c.upsertVariable(name, value, "*", true)
}

// SetEnvSecret writes a masked variable scoped to environment env. GitLab keys a
// variable by (key, environment_scope), so the scope participates in create,
// update and delete — the "not isomorphic to GitHub infra-<env>" caveat.
func (c *GitLabClient) SetEnvSecret(env, name, value string) error {
	return c.upsertVariable(name, value, env, true)
}

// SetVariable writes an unmasked (plaintext) project-wide CI/CD variable.
func (c *GitLabClient) SetVariable(name, value string) error {
	return c.upsertVariable(name, value, "*", false)
}

// DeleteEnvSecret removes a variable scoped to env. Idempotent: a 404 is success.
func (c *GitLabClient) DeleteEnvSecret(env, name string) error {
	u := fmt.Sprintf("%s/projects/%s/variables/%s?filter[environment_scope]=%s",
		c.apiBase, c.projectPath(), url.PathEscape(name), url.QueryEscape(env))
	req, err := http.NewRequest(http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	c.auth(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("delete variable %s: %w", name, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("delete variable %s: HTTP %d: %s", name, resp.StatusCode, readErr(resp))
	}
}

// upsertVariable PUTs the (key, scope) variable, falling back to POST-create on a
// 404 — the same update-then-create idempotency the GitHub writer uses.
func (c *GitLabClient) upsertVariable(name, value, scope string, masked bool) error {
	form := url.Values{}
	form.Set("value", value)
	form.Set("masked", fmt.Sprintf("%t", masked))
	form.Set("environment_scope", scope)

	putURL := fmt.Sprintf("%s/projects/%s/variables/%s?filter[environment_scope]=%s",
		c.apiBase, c.projectPath(), url.PathEscape(name), url.QueryEscape(scope))
	code, err := c.doForm(http.MethodPut, putURL, form)
	if err != nil {
		return err
	}
	if code == http.StatusOK {
		return nil
	}
	if code != http.StatusNotFound {
		return fmt.Errorf("update variable %s: HTTP %d", name, code)
	}
	// Absent → create it (key is part of the POST body).
	form.Set("key", name)
	postURL := fmt.Sprintf("%s/projects/%s/variables", c.apiBase, c.projectPath())
	code, err = c.doForm(http.MethodPost, postURL, form)
	if err != nil {
		return err
	}
	if code != http.StatusCreated && code != http.StatusOK {
		return fmt.Errorf("create variable %s: HTTP %d", name, code)
	}
	return nil
}

func (c *GitLabClient) doForm(method, u string, form url.Values) (int, error) {
	req, err := http.NewRequest(method, u, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// --- TokenMinter / TokenRotator: project access tokens ---

// MintEphemeral creates a project access token with the given scopes, expiring
// ttlSeconds from now (GitLab expires_at is a date, so the effective expiry is
// end-of-day). Returns the token value.
func (c *GitLabClient) MintEphemeral(scopes []string, ttlSeconds int) (string, error) {
	form := url.Values{}
	form.Set("name", "llz-ephemeral")
	form.Set("expires_at", c.expiresAt(ttlSeconds))
	for _, s := range scopes {
		form.Add("scopes[]", s)
	}
	u := fmt.Sprintf("%s/projects/%s/access_tokens", c.apiBase, c.projectPath())
	return c.tokenFromForm(http.MethodPost, u, form)
}

// RotateSelf rotates the token this client authenticates with (requires the
// self_rotate scope), returning the new value. This is the self-renewing path
// that makes a GitLab instance carry no permanent root secret.
func (c *GitLabClient) RotateSelf(ttlSeconds int) (string, error) {
	u := fmt.Sprintf("%s/projects/%s/access_tokens/self/rotate", c.apiBase, c.projectPath())
	form := url.Values{"expires_at": {c.expiresAt(ttlSeconds)}}
	return c.tokenFromForm(http.MethodPost, u, form)
}

// Rotate rotates a specific project access token by id.
func (c *GitLabClient) Rotate(tokenID string, ttlSeconds int) (string, error) {
	u := fmt.Sprintf("%s/projects/%s/access_tokens/%s/rotate", c.apiBase, c.projectPath(), url.PathEscape(tokenID))
	form := url.Values{"expires_at": {c.expiresAt(ttlSeconds)}}
	return c.tokenFromForm(http.MethodPost, u, form)
}

// TokenExpiry reports when token expires, authenticating AS that token against
// the self-introspection endpoint. GitLab returns a date; the expiry is its
// midnight-UTC unix timestamp.
func (c *GitLabClient) TokenExpiry(token string) (int64, error) {
	u := fmt.Sprintf("%s/projects/%s/access_tokens/self", c.apiBase, c.projectPath())
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("PRIVATE-TOKEN", token) // introspect the passed token, not c.token
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("token introspection: HTTP %d: %s", resp.StatusCode, readErr(resp))
	}
	var r struct {
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return 0, err
	}
	if r.ExpiresAt == "" {
		return 0, fmt.Errorf("token introspection: no expires_at (non-expiring token?)")
	}
	t, err := time.Parse("2006-01-02", r.ExpiresAt)
	if err != nil {
		return 0, fmt.Errorf("parse expires_at %q: %w", r.ExpiresAt, err)
	}
	return t.Unix(), nil
}

// expiresAt renders now+ttl as the YYYY-MM-DD GitLab expects.
func (c *GitLabClient) expiresAt(ttlSeconds int) string {
	return c.now().Add(time.Duration(ttlSeconds) * time.Second).UTC().Format("2006-01-02")
}

// tokenFromForm POSTs form and returns the .token field of the JSON response.
func (c *GitLabClient) tokenFromForm(method, u string, form url.Values) (string, error) {
	req, err := http.NewRequest(method, u, bytes.NewReader([]byte(form.Encode())))
	if err != nil {
		return "", err
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("access-token op: HTTP %d: %s", resp.StatusCode, readErr(resp))
	}
	var r struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if r.Token == "" {
		return "", fmt.Errorf("access-token op: response had no token")
	}
	return r.Token, nil
}

func readErr(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return strings.TrimSpace(string(b))
}
