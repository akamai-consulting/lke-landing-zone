package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

func TestGHSetRepoSecretNativeRoundTrip(t *testing.T) {
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var gotPut struct {
		EncryptedValue string `json:"encrypted_value"`
		KeyID          string `json:"key_id"`
	}
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/actions/secrets/public-key"):
			_ = json.NewEncoder(w).Encode(map[string]string{
				"key_id": "key-1",
				"key":    base64.StdEncoding.EncodeToString(pub[:]),
			})
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/actions/secrets/"):
			gotPath = r.URL.Path
			if err := json.NewDecoder(r.Body).Decode(&gotPut); err != nil {
				t.Errorf("PUT body not JSON: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	prev := ghAPIBase
	ghAPIBase = srv.URL
	t.Cleanup(func() { ghAPIBase = prev })
	t.Setenv("GH_TOKEN", "ghp_test")
	t.Setenv("GH_REPO", "acme/platform")

	if err := ghSetRepoSecretNative("HARBOR_PASSWORD", "s3cr3t"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repos/acme/platform/actions/secrets/HARBOR_PASSWORD" {
		t.Errorf("PUT path = %q", gotPath)
	}
	if gotAuth != "Bearer ghp_test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotPut.KeyID != "key-1" {
		t.Errorf("key_id = %q", gotPut.KeyID)
	}
	// The sealed box must decrypt back to the plaintext with the private key.
	sealed, err := base64.StdEncoding.DecodeString(gotPut.EncryptedValue)
	if err != nil {
		t.Fatal(err)
	}
	plain, ok := box.OpenAnonymous(nil, sealed, pub, priv)
	if !ok || string(plain) != "s3cr3t" {
		t.Errorf("sealed box decrypted to %q ok=%v, want s3cr3t", plain, ok)
	}
}

func TestGHSetRepoSecretNativeErrors(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GH_REPO", "")
	if err := ghSetRepoSecretNative("X", "v"); err == nil || !strings.Contains(err.Error(), "GH_TOKEN and GH_REPO") {
		t.Errorf("err = %v, want missing-env refusal", err)
	}

	// Public-key fetch failure surfaces.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	prev := ghAPIBase
	ghAPIBase = srv.URL
	t.Cleanup(func() { ghAPIBase = prev })
	t.Setenv("GH_TOKEN", "ghp_test")
	t.Setenv("GH_REPO", "acme/platform")
	if err := ghSetRepoSecretNative("X", "v"); err == nil || !strings.Contains(err.Error(), "public key") {
		t.Errorf("err = %v, want public-key fetch failure", err)
	}
}

func TestGHActionsPublicKeyValidatesLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"key_id": "k", "key": base64.StdEncoding.EncodeToString([]byte("short")),
		})
	}))
	t.Cleanup(srv.Close)
	_, _, err := ghActionsPublicKey(&http.Client{}, "tok", srv.URL+"/secrets")
	if err == nil || !strings.Contains(err.Error(), "32") {
		t.Errorf("err = %v, want 32-byte validation", err)
	}
}

// The environment-secret path resolves the numeric repo id, fetches the env public
// key, and PUTs to the environment endpoint — the round-trip the in-cluster broad-PAT
// rotator uses to write LINODE_API_TOKEN back to each infra-<deployment> environment.
func TestGHSetEnvSecretNativeRoundTrip(t *testing.T) {
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var gotPutPath string
	var gotPut struct {
		EncryptedValue string `json:"encrypted_value"`
		KeyID          string `json:"key_id"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/platform":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/secrets/public-key"):
			_ = json.NewEncoder(w).Encode(map[string]string{"key_id": "env-key", "key": base64.StdEncoding.EncodeToString(pub[:])})
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/environments/"):
			gotPutPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotPut)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	prev := ghAPIBase
	ghAPIBase = srv.URL
	t.Cleanup(func() { ghAPIBase = prev })
	t.Setenv("GH_TOKEN", "ghp_test")
	t.Setenv("GH_REPO", "acme/platform")

	if err := ghSetEnvSecretNative("LINODE_API_TOKEN", "infra-primary", "new-pat-value"); err != nil {
		t.Fatal(err)
	}
	// Environment endpoint keys off the numeric repo id (4242), not owner/name.
	if gotPutPath != "/repositories/4242/environments/infra-primary/secrets/LINODE_API_TOKEN" {
		t.Errorf("PUT path = %q", gotPutPath)
	}
	if gotPut.KeyID != "env-key" {
		t.Errorf("key_id = %q", gotPut.KeyID)
	}
	sealed, _ := base64.StdEncoding.DecodeString(gotPut.EncryptedValue)
	plain, ok := box.OpenAnonymous(nil, sealed, pub, priv)
	if !ok || string(plain) != "new-pat-value" {
		t.Errorf("sealed box decrypted to %q ok=%v", plain, ok)
	}
}

// resolveGHAPIBase must honor $GITHUB_API (the bug: the write path used to
// ignore it and target api.github.com even on a GHES instance) and derive a
// GHES api base from a non-github.com GH_HOST, else default to github.com.
func TestResolveGHAPIBase(t *testing.T) {
	cases := []struct {
		name, githubAPI, ghHost, want string
	}{
		{"explicit override wins", "https://ghes.corp/api/v3", "", "https://ghes.corp/api/v3"},
		{"ghes from GH_HOST", "", "ghes.corp", "https://ghes.corp/api/v3"},
		{"github.com GH_HOST falls through", "", "github.com", "https://api.github.com"},
		{"default", "", "", "https://api.github.com"},
		{"GITHUB_API beats GH_HOST", "https://override/api/v3", "other.corp", "https://override/api/v3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("GITHUB_API", c.githubAPI)
			t.Setenv("GH_HOST", c.ghHost)
			if got := resolveGHAPIBase(); got != c.want {
				t.Errorf("resolveGHAPIBase() = %q, want %q", got, c.want)
			}
		})
	}
}
