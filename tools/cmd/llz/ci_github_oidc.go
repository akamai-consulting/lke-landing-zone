package main

// ci_github_oidc.go — mint a GitHub Actions OIDC JWT for OpenBao's jwt auth
// method. Replaces the long-lived AppRole secret_id (stashed in GitHub Actions
// secrets and rotated in-cluster via `gh secret set`) with a short-lived,
// per-run, repo-bound token: the workflow declares `permissions: id-token: write`
// and we exchange the resulting OIDC token for an OpenBao token via
// `auth/jwt/login` (role configured by `llz ci bao-configure`).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// oidcAudienceForRepo returns the OpenBao jwt-role audience for a
// GITHUB_REPOSITORY ("<owner>/<name>"): the owner's GitHub-OIDC default
// audience, matching the bound_audiences `llz ci bao-configure` pins on the role.
func oidcAudienceForRepo(ghRepo string) string {
	owner := ghRepo
	if i := strings.IndexByte(ghRepo, '/'); i > 0 {
		owner = ghRepo[:i]
	}
	return "https://github.com/" + owner
}

// githubActionsOIDCToken mints a GitHub Actions OIDC JWT for the given audience.
// Requires `permissions: id-token: write` on the job, which populates
// ACTIONS_ID_TOKEN_REQUEST_URL + ACTIONS_ID_TOKEN_REQUEST_TOKEN. httpGet is
// injectable for tests; nil uses the default client.
func githubActionsOIDCToken(audience string, httpGet func(*http.Request) (*http.Response, error)) (string, error) {
	reqURL := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")
	reqTok := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if reqURL == "" || reqTok == "" {
		return "", fmt.Errorf("ACTIONS_ID_TOKEN_REQUEST_URL/TOKEN not set — the job needs `permissions: id-token: write`")
	}
	u, err := url.Parse(reqURL)
	if err != nil {
		return "", fmt.Errorf("parse ACTIONS_ID_TOKEN_REQUEST_URL: %w", err)
	}
	q := u.Query()
	q.Set("audience", audience)
	u.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+reqTok)
	req.Header.Set("Accept", "application/json")

	if httpGet == nil {
		client := &http.Client{Timeout: 30 * time.Second}
		httpGet = client.Do
	}
	resp, err := httpGet(req)
	if err != nil {
		return "", fmt.Errorf("request GitHub OIDC token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub OIDC token request returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Value == "" {
		return "", fmt.Errorf("GitHub OIDC token response missing 'value'")
	}
	return out.Value, nil
}
