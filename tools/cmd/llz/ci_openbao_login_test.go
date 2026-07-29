package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// stubInClusterBaoClient replaces the pod→OpenBao mTLS transport for one test.
// The real one reads a client keypair off disk (the workload's llz-client-ca
// identity), which a unit test has no business minting — the keypair handling
// itself is covered by TestHTTPClientMTLS_Handshake in internal/openbao.
func stubInClusterBaoClient(t *testing.T, c *http.Client) {
	t.Helper()
	prev := inClusterBaoHTTPClient
	inClusterBaoHTTPClient = func() (*http.Client, error) { return c, nil }
	t.Cleanup(func() { inClusterBaoHTTPClient = prev })
}

// TestOpenBaoLoginRequiresClientIdentity: with no client certificate mounted,
// the login must fail with an actionable error rather than silently falling back
// to an unauthenticated transport (which is what OPENBAO_SKIP_VERIFY used to do).
func TestOpenBaoLoginRequiresClientIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENBAO_CA_FILE", filepath.Join(dir, "absent-ca.crt"))
	t.Setenv("OPENBAO_CLIENT_CERT_FILE", filepath.Join(dir, "absent.crt"))
	t.Setenv("OPENBAO_CLIENT_KEY_FILE", filepath.Join(dir, "absent.key"))
	// Defeat the memoization so this test sees a fresh read of the env above.
	prev := inClusterBaoHTTPClient
	inClusterBaoHTTPClient = newInClusterBaoHTTPClient()
	t.Cleanup(func() { inClusterBaoHTTPClient = prev })

	err := runOpenBaoLogin(globalOpts{}, "kubernetes", "reconciler", "https://x", "kubernetes", "", "OPENBAO_TOKEN")
	if err == nil {
		t.Fatal("expected an error when no client identity is mounted")
	}
	if !strings.Contains(err.Error(), "OpenBao CA") && !strings.Contains(err.Error(), "client certificate") {
		t.Errorf("error should name the missing TLS material, got: %v", err)
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

	stubInClusterBaoClient(t, srv.Client())
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

// TestInClusterBaoClientRetriesAfterFailure is the regression test for the
// cold-start trap: #358 mounts the CA bundle `optional` on the reconciler, so
// the file can be absent on the first OpenBao pass. A sync.OnceValues memo would
// cache that failure permanently, and the reconciler's liveness probe never
// touches OpenBao — the pod would never recover without a manual restart.
//
// Asserts the constructor retries while the material is missing and starts
// succeeding once it lands, without a process restart.
func TestInClusterBaoClientRetriesAfterFailure(t *testing.T) {
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	crt := filepath.Join(dir, "tls.crt")
	key := filepath.Join(dir, "tls.key")
	t.Setenv("OPENBAO_CA_FILE", ca)
	t.Setenv("OPENBAO_CLIENT_CERT_FILE", crt)
	t.Setenv("OPENBAO_CLIENT_KEY_FILE", key)

	get := newInClusterBaoHTTPClient()

	// Cold start: nothing mounted yet.
	if _, err := get(); err == nil {
		t.Fatal("expected an error while the TLS material is absent")
	}

	// cert-manager writes the Secret; kubelet materialises the files.
	caPEM, certPEM, keyPEM := testClientPKI(t)
	for f, b := range map[string][]byte{ca: caPEM, crt: certPEM, key: keyPEM} {
		if err := os.WriteFile(f, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	c, err := get()
	if err != nil {
		t.Fatalf("should recover once the material lands, got: %v", err)
	}
	if c == nil {
		t.Fatal("nil client")
	}
	// And the success is cached from then on.
	c2, err := get()
	if err != nil || c2 != c {
		t.Errorf("expected the built client to be cached, got c2=%p err=%v", c2, err)
	}
}

// testClientPKI mints a throwaway CA plus a client leaf signed by it, as PEM.
// Self-contained so the test does not depend on cert-manager or on fixtures.
func testClientPKI(t *testing.T) (caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	kb, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return caPEM, certPEM, keyPEM
}
