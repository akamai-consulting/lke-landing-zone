package forge

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

// OIDCAudienceForRepo returns the OpenBao jwt-role audience for a repo slug
// ("<owner>/<name>"): the owner's OIDC default audience, matching the
// bound_audiences `llz ci bao-configure` pins on the role. Forge-derived so the
// minted audience and the role's bound audience stay in lockstep across GitHub /
// GHES / GitLab; defaults to GitHub. See docs/designs/forge-abstraction.md.
func OIDCAudienceForRepo(ghRepo string) string {
	owner := ghRepo
	if i := strings.IndexByte(ghRepo, '/'); i > 0 {
		owner = ghRepo[:i]
	}
	f, err := FromEnv()
	if err != nil {
		return "https://github.com/" + owner // conservative fallback
	}
	return AudienceFor(f, owner)
}

// ActionsOIDCToken mints a GitHub Actions OIDC JWT for the given audience.
// Requires `permissions: id-token: write` on the job, which populates
// ACTIONS_ID_TOKEN_REQUEST_URL + ACTIONS_ID_TOKEN_REQUEST_TOKEN. httpGet is
// injectable for tests; nil uses the default client.
func ActionsOIDCToken(audience string, httpGet func(*http.Request) (*http.Response, error)) (string, error) {
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

// FromEnv resolves the instance's forge from the environment, defaulting to
// GitHub.
//
// IT MOVED HERE FROM cmd/llz/forge_env.go, which is where it should always have
// been: it constructs a Forge out of two env vars this package already defines the
// meaning of, and package main was the only place that could not test it without
// building a whole command. The four-line copy in forge_env.go is gone; that file
// keeps only the wiring that genuinely needs main's flags.
func FromEnv() (Forge, error) {
	flavor := Flavor(os.Getenv("LLZ_FORGE"))
	if flavor == "" {
		flavor = GitHub
	}
	return New(flavor, os.Getenv("LLZ_FORGE_HOST"))
}
