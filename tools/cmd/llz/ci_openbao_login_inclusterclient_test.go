package main

// The one test of the pair that is about openbao.RunCILogin, not about the client.
// It resets the memo through the exported constructor so the env vars it sets are
// actually read — filename-as-subject again, in miniature.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
)

func TestOpenBaoLoginRequiresClientIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENBAO_CA_FILE", filepath.Join(dir, "absent-ca.crt"))
	t.Setenv("OPENBAO_CLIENT_CERT_FILE", filepath.Join(dir, "absent.crt"))
	t.Setenv("OPENBAO_CLIENT_KEY_FILE", filepath.Join(dir, "absent.key"))
	// Defeat the memoization so this test sees a fresh read of the env above.
	prev := openbao.InClusterHTTPClient
	openbao.InClusterHTTPClient = openbao.NewInClusterHTTPClient()
	t.Cleanup(func() { openbao.InClusterHTTPClient = prev })

	err := openbao.RunCILogin(false, "kubernetes", "reconciler", "https://x", "kubernetes", "", "OPENBAO_TOKEN")
	if err == nil {
		t.Fatal("expected an error when no client identity is mounted")
	}
	if !strings.Contains(err.Error(), "OpenBao CA") && !strings.Contains(err.Error(), "client certificate") {
		t.Errorf("error should name the missing TLS material, got: %v", err)
	}
}
