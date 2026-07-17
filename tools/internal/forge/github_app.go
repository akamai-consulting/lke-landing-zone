package forge

// github_app.go — the GitHub App TokenMinter: sign a short-lived App JWT with
// the App's RSA private key and exchange it for a ≤1h installation access token.
// This is the Phase 4 path that removes the PAT expiry cliff from the GitHub
// family (App private keys never expire; the effective credential is the
// ephemeral installation token) and works on GHES unchanged because the API base
// comes from the forge.
//
// The App private key is the irreducible permanent secret — there is no API to
// rotate it (UI-only, on github.com and GHES alike), which is exactly why the
// GitHub family does NOT implement TokenRotator. It is escrowed like
// OPENBAO_SEAL_KEY. See docs/designs/forge-abstraction.md (Phase 4).
//
// JWT is signed with crypto/rsa (RS256) directly rather than pulling in a JWT
// dependency. End-to-end validation is gated on a real App + OpenBao GitHub
// secrets engine; the unit tests verify the signed JWT against the public key
// and the installation-token exchange against httptest.

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"time"
)

// GitHubAppMinter mints installation access tokens for one App installation.
// It implements TokenMinter; the scopes/ttl of MintEphemeral are not honored —
// GitHub installation tokens are scoped by the installation's configured
// permissions and fixed at ~1h — so callers wanting the expiry use
// InstallationToken directly.
type GitHubAppMinter struct {
	apiBase        string
	appID          string
	installationID string
	key            *rsa.PrivateKey
	client         *http.Client
	now            func() time.Time
}

var _ TokenMinter = (*GitHubAppMinter)(nil)

// NewGitHubAppMinter builds a minter against a GitHub-family forge, for App
// appID installed as installationID, signing with the PEM-encoded RSA private
// key (PKCS#1 or PKCS#8).
func NewGitHubAppMinter(f Forge, appID, installationID string, privateKeyPEM []byte) (*GitHubAppMinter, error) {
	if _, ok := f.(githubFamily); !ok {
		return nil, fmt.Errorf("NewGitHubAppMinter: forge %q is not a GitHub family forge", f.Flavor())
	}
	if appID == "" || installationID == "" {
		return nil, fmt.Errorf("GitHub App minter needs an app id and an installation id")
	}
	key, err := parseRSAPrivateKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	return &GitHubAppMinter{
		apiBase:        f.APIBase(),
		appID:          appID,
		installationID: installationID,
		key:            key,
		client:         &http.Client{Timeout: 15 * time.Second},
		now:            time.Now,
	}, nil
}

// MintEphemeral satisfies TokenMinter; scopes and ttlSeconds are ignored (see
// type doc) and a full installation token is returned.
func (m *GitHubAppMinter) MintEphemeral(_ []string, _ int) (string, error) {
	tok, _, err := m.InstallationToken()
	return tok, err
}

// InstallationToken signs an App JWT and exchanges it for an installation access
// token, returning the token and its expiry.
func (m *GitHubAppMinter) InstallationToken() (string, time.Time, error) {
	jwt, err := m.signAppJWT()
	if err != nil {
		return "", time.Time{}, err
	}
	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", m.apiBase, m.installationID)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("installation token exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("installation token exchange: HTTP %d: %s", resp.StatusCode, readErr(resp))
	}
	var r struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", time.Time{}, err
	}
	if r.Token == "" {
		return "", time.Time{}, fmt.Errorf("installation token exchange: response had no token")
	}
	exp, _ := time.Parse(time.RFC3339, r.ExpiresAt) // zero time if absent/unparseable
	return r.Token, exp, nil
}

// signAppJWT builds the RS256-signed JWT GitHub requires for App auth: iss = app
// id, iat backdated 60s for clock skew, exp at +9m (GitHub's ceiling is 10m).
func (m *GitHubAppMinter) signAppJWT() (string, error) {
	now := m.now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": m.appID,
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := b64url(hb) + "." + b64url(cb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, m.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}
	return signingInput + "." + b64url(sig), nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// parseRSAPrivateKey accepts a PKCS#1 ("RSA PRIVATE KEY") or PKCS#8 ("PRIVATE
// KEY") PEM block — GitHub Apps historically issue PKCS#1.
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in App private key")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k8, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("App private key is neither PKCS#1 nor PKCS#8: %w", err)
	}
	rsaKey, ok := k8.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("App private key is not RSA (%T)", k8)
	}
	return rsaKey, nil
}
