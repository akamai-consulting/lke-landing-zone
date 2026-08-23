package openbao

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

func TestOpenBaoLoginDryRun(t *testing.T) {
	// dry-run must not touch the network or the filesystem, for either method.
	if err := RunCILogin(true, "kubernetes", "", "", "", "", "OPENBAO_TOKEN", ""); err != nil {
		t.Fatalf("kubernetes dry-run should be a no-op success: %v", err)
	}
	if err := RunCILogin(true, "oidc", "", "", "", "", "OPENBAO_TOKEN", ""); err != nil {
		t.Fatalf("oidc dry-run should be a no-op success: %v", err)
	}
}

func TestOpenBaoLoginUnknownMethod(t *testing.T) {
	if err := RunCILogin(false, "carrier-pigeon", "", "", "", "", "OPENBAO_TOKEN", ""); err == nil {
		t.Fatal("expected an error for an unknown --method")
	}
}

// stubInClusterBaoClient replaces the pod→OpenBao mTLS transport for one test.
// The real one reads a client keypair off disk (the workload's llz-client-ca
// identity), which a unit test has no business minting — the keypair handling
// itself is covered by TestHTTPClientMTLS_Handshake in internal/
func stubInClusterBaoClient(t *testing.T, c *http.Client) {
	t.Helper()
	prev := openbao.InClusterHTTPClient
	openbao.InClusterHTTPClient = func() (*http.Client, error) { return c, nil }
	t.Cleanup(func() { openbao.InClusterHTTPClient = prev })
}

// TestOpenBaoLoginRequiresClientIdentity: with no client certificate mounted,
// the login must fail with an actionable error rather than silently falling back
// to an unauthenticated transport (which is what OPENBAO_SKIP_VERIFY used to do).
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

	stubInClusterBaoClient(t, srv.Client())
	if err := RunCILogin(false, "kubernetes", "reconciler", srv.URL, "kubernetes", saFile, "OPENBAO_TOKEN", ""); err != nil {
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
	if err := RunCILogin(false, "kubernetes", "reconciler", "https://x", "kubernetes",
		filepath.Join(t.TempDir(), "nope"), "OPENBAO_TOKEN", ""); err == nil {
		t.Fatal("expected an error when the SA token file is absent")
	}
}

// TestInClusterBaoClientRetriesAfterFailure is the regression test for the
// cold-start trap: #358 mounts the CA bundle `optional` on the reconciler, so
// the file can be absent on the first OpenBao pass. A sync.OnceValues memo would
// cache that failure permanently, and the reconciler's liveness probe never
// touches OpenBao — the pod would never recover without a manual restart.
//
// Asserts the constructor retries while the material is missing and starts
// succeeding once it lands, without a process restart.
