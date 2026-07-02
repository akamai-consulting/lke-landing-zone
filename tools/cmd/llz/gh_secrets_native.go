package main

// gh_secrets_native.go — write GitHub Actions repo-level secrets over the REST
// API with libsodium sealed-box encryption (x/crypto/nacl/box.SealAnonymous),
// no `gh` binary. Needed by workloads on the slim distroless llz image (no
// shell, no gh) — specifically the in-cluster harbor-robot-provisioner CronJob,
// which publishes the Harbor robot credentials as the repo-level secrets a
// standby bootstrap seeds its OpenBao from (`llz ci seed-standby-harbor-robots`).
// CI-side callers keep using ghSecretSetStdin (commands.go): gh handles auth
// modes + GHES hosts that this deliberately does not.
//
// Auth/repo come from GH_TOKEN + GH_REPO (owner/name) — the same env contract
// the gh CLI uses, sourced in-cluster from the ESO-synced github-dispatch-token
// Secret + a copier-rendered literal.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/nacl/box"
)

// ghAPIBase is a seam so tests can point at an httptest server.
var ghAPIBase = "https://api.github.com"

// ghSetRepoSecretNative writes one repo-level Actions secret: fetches the
// repo's public key, seals the value (anonymous NaCl box — the libsodium
// crypto_box_seal GitHub requires), and PUTs it. Idempotent (PUT semantics).
func ghSetRepoSecretNative(name, value string) error {
	token, repo := os.Getenv("GH_TOKEN"), os.Getenv("GH_REPO")
	if token == "" || repo == "" {
		return fmt.Errorf("GH_TOKEN and GH_REPO must be set to write repo secret %s", name)
	}
	client := &http.Client{Timeout: 15 * time.Second}

	keyID, pubKey, err := ghRepoPublicKey(client, token, repo)
	if err != nil {
		return fmt.Errorf("fetch %s actions public key: %w", repo, err)
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
	req, err := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/repos/%s/actions/secrets/%s", ghAPIBase, repo, name), bytes.NewReader(body))
	if err != nil {
		return err
	}
	ghAuthHeaders(req, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("put repo secret %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("put repo secret %s: HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// ghRepoPublicKey fetches the repo's Actions public key (key_id + the 32-byte
// X25519 key GitHub base64-encodes).
func ghRepoPublicKey(client *http.Client, token, repo string) (keyID string, key *[32]byte, err error) {
	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/repos/%s/actions/secrets/public-key", ghAPIBase, repo), nil)
	if err != nil {
		return "", nil, err
	}
	ghAuthHeaders(req, token)
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
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

func ghAuthHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

// ghRepoSecretExistsNative reports whether a repo-level Actions secret exists
// (GitHub exposes secret METADATA without the value: 200 = exists, 404 = not).
// Lets the provisioner's steady state detect a lost/failed publication and
// re-publish from OpenBao without touching Harbor.
func ghRepoSecretExistsNative(name string) (bool, error) {
	token, repo := os.Getenv("GH_TOKEN"), os.Getenv("GH_REPO")
	if token == "" || repo == "" {
		return false, fmt.Errorf("GH_TOKEN and GH_REPO must be set to check repo secret %s", name)
	}
	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/repos/%s/actions/secrets/%s", ghAPIBase, repo, name), nil)
	if err != nil {
		return false, err
	}
	ghAuthHeaders(req, token)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
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
		return false, fmt.Errorf("get repo secret %s: HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(b)))
	}
}
