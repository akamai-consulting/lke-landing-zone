package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenBaoLoginDryRun(t *testing.T) {
	// dry-run must not touch the network or the filesystem, for either method.
	if err := runOpenBaoLogin(globalOpts{dryRun: true}, "kubernetes", "", "", "", "", "OPENBAO_TOKEN"); err != nil {
		t.Fatalf("kubernetes dry-run should be a no-op success: %v", err)
	}
	if err := runOpenBaoLogin(globalOpts{dryRun: true}, "oidc", "", "", "", "", "OPENBAO_TOKEN"); err != nil {
		t.Fatalf("oidc dry-run should be a no-op success: %v", err)
	}
}

func TestOpenBaoLoginUnknownMethod(t *testing.T) {
	if err := runOpenBaoLogin(globalOpts{}, "carrier-pigeon", "", "", "", "", "OPENBAO_TOKEN"); err == nil {
		t.Fatal("expected an error for an unknown --method")
	}
}

func TestOpenBaoLoginKubernetesExportsToken(t *testing.T) {
	// A fake OpenBao that accepts the kubernetes login and returns a token.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"auth":{"client_token":"s.k8s-token"}}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	saFile := filepath.Join(dir, "token")
	if err := os.WriteFile(saFile, []byte("sa-jwt-abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	ghEnv := filepath.Join(dir, "gh_env")
	t.Setenv("GITHUB_ENV", ghEnv)

	if err := runOpenBaoLogin(globalOpts{}, "kubernetes", "reconciler", srv.URL, "kubernetes", saFile, "OPENBAO_TOKEN"); err != nil {
		t.Fatalf("kubernetes login: %v", err)
	}
	got, err := os.ReadFile(ghEnv)
	if err != nil {
		t.Fatal(err)
	}
	if want := "OPENBAO_TOKEN=s.k8s-token\n"; string(got) != want {
		t.Errorf("$GITHUB_ENV = %q, want %q", string(got), want)
	}
}

func TestOpenBaoLoginKubernetesMissingSAToken(t *testing.T) {
	// No SA token file → a clear error (not a panic), the "not running in-cluster" case.
	if err := runOpenBaoLogin(globalOpts{}, "kubernetes", "reconciler", "https://x", "kubernetes",
		filepath.Join(t.TempDir(), "nope"), "OPENBAO_TOKEN"); err == nil {
		t.Fatal("expected an error when the SA token file is absent")
	}
}
