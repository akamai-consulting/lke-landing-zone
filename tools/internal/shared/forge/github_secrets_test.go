package forge

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

func mustWriter(t *testing.T, apiBase string) *GitHubSecretWriter {
	t.Helper()
	w, err := NewGitHubSecretWriter(apiBase, "ghp_test", "acme/platform")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestNewGitHubSecretWriter_Requires(t *testing.T) {
	for _, c := range []struct{ apiBase, token, repo string }{
		{"", "t", "o/r"}, {"https://x", "", "o/r"}, {"https://x", "t", ""},
	} {
		if _, err := NewGitHubSecretWriter(c.apiBase, c.token, c.repo); err == nil {
			t.Errorf("NewGitHubSecretWriter(%q,%q,%q) = nil err, want refusal", c.apiBase, c.token, c.repo)
		}
	}
}

func TestGitHubSecretWriter_RepoRoundTrip(t *testing.T) {
	pub, priv, _ := box.GenerateKey(rand.Reader)
	var gotPath, gotAuth, gotAPIVersion string
	var body struct {
		EncryptedValue string `json:"encrypted_value"`
		KeyID          string `json:"key_id"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAPIVersion = r.Header.Get("Authorization"), r.Header.Get("X-GitHub-Api-Version")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/actions/secrets/public-key"):
			_ = json.NewEncoder(w).Encode(map[string]string{"key_id": "key-1", "key": base64.StdEncoding.EncodeToString(pub[:])})
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/actions/secrets/"):
			gotPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	if err := mustWriter(t, srv.URL).SetRepoSecret("HARBOR_PASSWORD", "s3cr3t"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repos/acme/platform/actions/secrets/HARBOR_PASSWORD" {
		t.Errorf("PUT path = %q", gotPath)
	}
	if gotAuth != "Bearer ghp_test" || gotAPIVersion != "2022-11-28" {
		t.Errorf("headers = %q / %q", gotAuth, gotAPIVersion)
	}
	sealed, _ := base64.StdEncoding.DecodeString(body.EncryptedValue)
	if plain, ok := box.OpenAnonymous(nil, sealed, pub, priv); !ok || string(plain) != "s3cr3t" {
		t.Errorf("sealed box decrypted to %q ok=%v, want s3cr3t", plain, ok)
	}
}

func TestGitHubSecretWriter_EnvRoundTripUsesNumericID(t *testing.T) {
	pub, _, _ := box.GenerateKey(rand.Reader)
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/platform":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242})
		case strings.HasSuffix(r.URL.Path, "/secrets/public-key"):
			_ = json.NewEncoder(w).Encode(map[string]string{"key_id": "env-key", "key": base64.StdEncoding.EncodeToString(pub[:])})
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/environments/"):
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	if err := mustWriter(t, srv.URL).SetEnvSecret("infra-primary", "LINODE_API_TOKEN", "v"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repositories/4242/environments/infra-primary/secrets/LINODE_API_TOKEN" {
		t.Errorf("PUT path = %q (env endpoint must key off numeric id 4242)", gotPath)
	}
}

func TestGitHubSecretWriter_PublicKeyLengthValidated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"key_id": "k", "key": base64.StdEncoding.EncodeToString([]byte("short"))})
	}))
	defer srv.Close()
	err := mustWriter(t, srv.URL).SetRepoSecret("X", "v")
	if err == nil || !strings.Contains(err.Error(), "32") {
		t.Errorf("err = %v, want 32-byte key validation", err)
	}
}

func TestGitHubSecretWriter_DeleteEnvIdempotent(t *testing.T) {
	// A 404 (already gone) must be treated as success so teardown is re-runnable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/platform" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if err := mustWriter(t, srv.URL).DeleteEnvSecret("infra-primary", "GONE"); err != nil {
		t.Errorf("delete of an absent secret should succeed, got %v", err)
	}
}

func TestGitHubSecretWriter_SetVariableCreatesWhenAbsent(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == http.MethodPatch { // not there yet
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated) // POST create
	}))
	defer srv.Close()
	if err := mustWriter(t, srv.URL).SetVariable("APPS_REPO_REVISION", "abc"); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 2 || methods[0] != http.MethodPatch || methods[1] != http.MethodPost {
		t.Errorf("methods = %v, want [PATCH POST] (update-then-create)", methods)
	}
}

func TestGitHubSecretWriter_RepoSecretExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/PRESENT") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	w := mustWriter(t, srv.URL)
	if ok, err := w.RepoSecretExists("PRESENT"); err != nil || !ok {
		t.Errorf("PRESENT: ok=%v err=%v, want true/nil", ok, err)
	}
	if ok, err := w.RepoSecretExists("ABSENT"); err != nil || ok {
		t.Errorf("ABSENT: ok=%v err=%v, want false/nil", ok, err)
	}
}
