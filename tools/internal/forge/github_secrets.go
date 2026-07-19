package forge

// github_secrets.go — the GitHub-family SecretWriter: write Actions secrets and
// variables over the REST API with libsodium sealed-box encryption (the
// crypto_box_seal GitHub requires), no `gh` binary. This is the in-cluster path,
// for workloads on the slim distroless llz image (no shell, no gh) — the Harbor
// robot provisioner and the broad-PAT rotator. CI-side callers keep using `gh`
// (it handles interactive auth modes this deliberately does not).
//
// This is where the Phase 2 GitHub implementation of the SecretWriter capability
// lives; cmd/llz/gh_secrets_native.go is now a thin env-sourced caller of it. The
// API base is passed in explicitly rather than taken from a Forge so the caller
// can supply an env-resolved host (GITHUB_API / GH_HOST) or a test server URL.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/crypto/nacl/box"
)

// GitHubSecretWriter implements SecretWriter (and a repo-secret existence probe)
// for any GitHub-family forge. Construct it with NewGitHubSecretWriter.
type GitHubSecretWriter struct {
	apiBase string // REST root, e.g. https://api.github.com or https://ghes.corp/api/v3
	token   string
	repo    string // owner/name
	client  *http.Client
}

var _ SecretWriter = (*GitHubSecretWriter)(nil)

// NewGitHubSecretWriter builds a writer against apiBase (an env-resolved host or
// a test server), authenticating as token, targeting the owner/name repo. It is
// GitHub-family only; GitLab's SecretWriter is a separate implementation (masked
// CI/CD variables, no sealed box).
func NewGitHubSecretWriter(apiBase, token, repo string) (*GitHubSecretWriter, error) {
	if apiBase == "" || token == "" || repo == "" {
		return nil, fmt.Errorf("GitHub secret writer needs apiBase, token and an owner/name repo")
	}
	return &GitHubSecretWriter{
		apiBase: apiBase,
		token:   token,
		repo:    repo,
		client:  &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// SetRepoSecret writes one repo-level Actions secret (sealed-box + idempotent PUT).
func (w *GitHubSecretWriter) SetRepoSecret(name, value string) error {
	base := fmt.Sprintf("%s/repos/%s/actions/secrets", w.apiBase, w.repo)
	return w.sealAndPut(base, name, value)
}

// SetEnvSecret writes one environment-scoped Actions secret (the infra-<env>
// copies the workflows read). Environment-secret endpoints key off the NUMERIC
// repository id, so it resolves that first.
func (w *GitHubSecretWriter) SetEnvSecret(env, name, value string) error {
	id, err := w.repoID()
	if err != nil {
		return fmt.Errorf("resolve repo id for %s: %w", w.repo, err)
	}
	base := fmt.Sprintf("%s/repositories/%d/environments/%s/secrets", w.apiBase, id, env)
	return w.sealAndPut(base, name, value)
}

// DeleteEnvSecret removes one environment-scoped Actions secret. Idempotent: a
// 404 (already gone) is treated as success, so a teardown that runs twice is safe.
func (w *GitHubSecretWriter) DeleteEnvSecret(env, name string) error {
	id, err := w.repoID()
	if err != nil {
		return fmt.Errorf("resolve repo id for %s: %w", w.repo, err)
	}
	url := fmt.Sprintf("%s/repositories/%d/environments/%s/secrets/%s", w.apiBase, id, env, name)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	w.auth(req)
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("delete env secret %s: %w", name, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete env secret %s: HTTP %d: %s", name, resp.StatusCode, string(b))
	}
}

// SetVariable writes one repo-level Actions variable (plaintext, not a secret).
// Idempotent: PATCH the existing variable, or POST to create it if absent.
func (w *GitHubSecretWriter) SetVariable(name, value string) error {
	body, err := json.Marshal(map[string]string{"name": name, "value": value})
	if err != nil {
		return err
	}
	// PATCH the existing variable first.
	patchURL := fmt.Sprintf("%s/repos/%s/actions/variables/%s", w.apiBase, w.repo, name)
	req, err := http.NewRequest(http.MethodPatch, patchURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	w.auth(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("patch variable %s: %w", name, err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("patch variable %s: HTTP %d", name, resp.StatusCode)
	}
	// Absent → create it.
	postURL := fmt.Sprintf("%s/repos/%s/actions/variables", w.apiBase, w.repo)
	req, err = http.NewRequest(http.MethodPost, postURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	w.auth(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err = w.client.Do(req)
	if err != nil {
		return fmt.Errorf("create variable %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create variable %s: HTTP %d: %s", name, resp.StatusCode, string(b))
	}
	return nil
}

// RepoSecretExists reports whether a repo-level Actions secret exists (GitHub
// exposes secret metadata without the value: 200 = exists, 404 = not). Lets the
// provisioner detect a lost publication and re-publish without touching Harbor.
func (w *GitHubSecretWriter) RepoSecretExists(name string) (bool, error) {
	url := fmt.Sprintf("%s/repos/%s/actions/secrets/%s", w.apiBase, w.repo, name)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	w.auth(req)
	resp, err := w.client.Do(req)
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
		b, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("get repo secret %s: HTTP %d: %s", name, resp.StatusCode, string(b))
	}
}

// sealAndPut fetches the Actions public key at secretsBase/public-key, seals
// value against it (anonymous NaCl box), and PUTs it to secretsBase/name.
func (w *GitHubSecretWriter) sealAndPut(secretsBase, name, value string) error {
	keyID, pubKey, err := w.actionsPublicKey(secretsBase)
	if err != nil {
		return fmt.Errorf("fetch actions public key: %w", err)
	}
	sealed, err := box.SealAnonymous(nil, []byte(value), pubKey, rand.Reader)
	if err != nil {
		return fmt.Errorf("seal secret %s: %w", name, err)
	}
	body, err := json.Marshal(map[string]string{
		"encrypted_value": base64.StdEncoding.EncodeToString(sealed),
		"key_id":          keyID,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, secretsBase+"/"+name, bytes.NewReader(body))
	if err != nil {
		return err
	}
	w.auth(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("put secret %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("put secret %s: HTTP %d: %s", name, resp.StatusCode, string(b))
	}
	return nil
}

// repoID resolves owner/name → the numeric repository id the environment-secret
// endpoints require.
func (w *GitHubSecretWriter) repoID() (int64, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/repos/%s", w.apiBase, w.repo), nil)
	if err != nil {
		return 0, err
	}
	w.auth(req)
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	var r struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return 0, err
	}
	if r.ID == 0 {
		return 0, fmt.Errorf("repo %s: response missing .id", w.repo)
	}
	return r.ID, nil
}

// actionsPublicKey fetches an Actions public key (repo- or env-level) from
// secretsBase/public-key: key_id + the 32-byte X25519 key GitHub base64-encodes.
func (w *GitHubSecretWriter) actionsPublicKey(secretsBase string) (keyID string, key *[32]byte, err error) {
	req, err := http.NewRequest(http.MethodGet, secretsBase+"/public-key", nil)
	if err != nil {
		return "", nil, err
	}
	w.auth(req)
	resp, err := w.client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	var pk struct {
		KeyID string `json:"key_id"`
		Key   string `json:"key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pk); err != nil {
		return "", nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(pk.Key)
	if err != nil {
		return "", nil, fmt.Errorf("decode public key: %w", err)
	}
	if len(raw) != 32 {
		return "", nil, fmt.Errorf("public key is %d bytes, want 32", len(raw))
	}
	var k [32]byte
	copy(k[:], raw)
	return pk.KeyID, &k, nil
}

func (w *GitHubSecretWriter) auth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}
