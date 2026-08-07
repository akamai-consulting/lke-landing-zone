package openbao

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHTTPClientLoopback(t *testing.T) {
	c := HTTPClientLoopback(7 * time.Second)
	if c == nil {
		t.Fatal("HTTPClientLoopback returned nil")
	}
	if c.Timeout != 7*time.Second {
		t.Errorf("timeout = %v, want 7s", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", c.Transport)
	}
	if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("expected InsecureSkipVerify=true")
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS1.2", tr.TLSClientConfig.MinVersion)
	}
}

func TestHTTPClientWithCA_BadBundle(t *testing.T) {
	if _, err := HTTPClientWithCA([]byte("not a cert"), time.Second); err == nil {
		t.Error("expected an error for an invalid CA bundle")
	}
}

func TestHTTPClientMTLS_BadInputs(t *testing.T) {
	ca, _, _ := testCA(t, "ca")
	_, certPEM, keyPEM := testLeaf(t, "client")

	t.Run("bad CA bundle", func(t *testing.T) {
		if _, err := HTTPClientMTLS([]byte("nope"), certPEM, keyPEM, time.Second); err == nil {
			t.Error("expected an error for an invalid CA bundle")
		}
	})
	t.Run("bad keypair", func(t *testing.T) {
		if _, err := HTTPClientMTLS(ca, []byte("nope"), []byte("nope"), time.Second); err == nil {
			t.Error("expected an error for an invalid client keypair")
		}
	})
	t.Run("cert without key", func(t *testing.T) {
		if _, err := HTTPClientMTLS(ca, certPEM, nil, time.Second); err == nil {
			t.Error("expected an error when the key is missing")
		}
	})
}

// TestHTTPClientMTLS_Handshake is the real assertion behind the mTLS change: a
// server that REQUIRES and verifies a client certificate (the posture OpenBao's
// listener now runs, tls_require_and_verify_client_cert) accepts a client built
// by HTTPClientMTLS and rejects one built without a client identity.
//
// Without this, "we enabled mTLS" is only asserted by YAML nobody executes.
func TestHTTPClientMTLS_Handshake(t *testing.T) {
	caPEM, caCert, caKey := testCA(t, "llz-client-ca-test")

	// Server cert, self-signed, so the client needs its own root to verify it.
	srvCAPEM, srvCACert, srvCAKey := testCA(t, "openbao-ca-test")
	srvCert := issue(t, srvCACert, srvCAKey, "127.0.0.1", true)

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("client CA pool")
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"auth":{"client_token":"s.ok"}}`))
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{srvCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert, // the OpenBao listener's posture
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()

	t.Run("client with an identity is accepted", func(t *testing.T) {
		_, certPEM, keyPEM := issuePEM(t, caCert, caKey, "reconciler", false)
		c, err := HTTPClientMTLS(srvCAPEM, certPEM, keyPEM, 5*time.Second)
		if err != nil {
			t.Fatalf("build client: %v", err)
		}
		tok, err := KubernetesLogin(context.Background(), c, srv.URL, "kubernetes", "reconciler", "jwt")
		if err != nil {
			t.Fatalf("mTLS login: %v", err)
		}
		if tok != "s.ok" {
			t.Errorf("token = %q, want s.ok", tok)
		}
	})

	t.Run("client without an identity is rejected", func(t *testing.T) {
		// Verifies the server cert but presents nothing — what every LLZ workload
		// did before this change (HTTPClientWithCA / skip-verify). Must now fail.
		c, err := HTTPClientWithCA(srvCAPEM, 5*time.Second)
		if err != nil {
			t.Fatalf("build client: %v", err)
		}
		if _, err := KubernetesLogin(context.Background(), c, srv.URL, "kubernetes", "reconciler", "jwt"); err == nil {
			t.Error("expected the handshake to fail without a client certificate")
		}
	})

	t.Run("client from the wrong CA is rejected", func(t *testing.T) {
		otherCACert, otherCAKey := func() (*x509.Certificate, *ecdsa.PrivateKey) {
			_, c, k := testCA(t, "not-the-client-ca")
			return c, k
		}()
		_, certPEM, keyPEM := issuePEM(t, otherCACert, otherCAKey, "impostor", false)
		c, err := HTTPClientMTLS(srvCAPEM, certPEM, keyPEM, 5*time.Second)
		if err != nil {
			t.Fatalf("build client: %v", err)
		}
		if _, err := KubernetesLogin(context.Background(), c, srv.URL, "kubernetes", "reconciler", "jwt"); err == nil {
			t.Error("expected the handshake to fail for a cert from an untrusted CA")
		}
	})
}

// ── test PKI helpers ─────────────────────────────────────────────────────────

func testCA(t *testing.T, cn string) ([]byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), cert, key
}

func issuePEM(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, server bool) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.DNSNames = []string{"localhost"}
		tmpl.IPAddresses = append(tmpl.IPAddresses, parseIP(t, cn))
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair, certPEM, keyPEM
}

func issue(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, server bool) tls.Certificate {
	t.Helper()
	pair, _, _ := issuePEM(t, ca, caKey, cn, server)
	return pair
}

func testLeaf(t *testing.T, cn string) (tls.Certificate, []byte, []byte) {
	t.Helper()
	_, caCert, caKey := testCA(t, cn+"-ca")
	return issuePEM(t, caCert, caKey, cn, false)
}

// TestJWTLogin_UnparseableBody covers the decode-error branch (a 2xx with a body
// that isn't the expected JSON shape).
func TestJWTLogin_UnparseableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	if _, err := JWTLogin(context.Background(), srv.Client(), srv.URL, "platform-ci", "j"); err == nil {
		t.Error("expected a parse error on a non-JSON body")
	}
}

// TestJWTLogin_Unreachable covers the httpClient.Do error branch.
func TestJWTLogin_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now → Do returns a connection error
	if _, err := JWTLogin(context.Background(), http.DefaultClient, url, "platform-ci", "j"); err == nil {
		t.Error("expected a transport error when OpenBao is unreachable")
	}
}

// parseIP is a tiny helper so a server leaf can carry a 127.0.0.1 SAN (the same
// SAN the real openbao-tls cert now carries for the loopback listener).
func parseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("not an IP: %s", s)
	}
	return ip
}

// TestHTTPClientMTLSFromFiles_PicksUpRotation is the regression test for the
// defect this constructor exists to prevent: a long-lived process that captured
// its keypair at startup keeps presenting the OLD certificate after cert-manager
// renews, and roughly 90 days later every handshake fails with nothing to
// restart the pod.
//
// It writes a keypair, builds a client, then REPLACES the files with a keypair
// from a CA the server does not trust. If the material were captured once, the
// second call would still succeed. It must fail — proving the files are re-read.
func TestHTTPClientMTLSFromFiles_PicksUpRotation(t *testing.T) {
	caPEM, caCert, caKey := testCA(t, "rotating-client-ca")
	srvCAPEM, srvCACert, srvCAKey := testCA(t, "server-ca")

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("client CA pool")
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"auth":{"client_token":"s.ok"}}`))
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{issue(t, srvCACert, srvCAKey, "127.0.0.1", true)},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.crt")
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")
	write := func(cert, key []byte) {
		t.Helper()
		if err := os.WriteFile(certFile, cert, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyFile, key, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(caFile, srvCAPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	_, goodCert, goodKey := issuePEM(t, caCert, caKey, "reconciler", false)
	write(goodCert, goodKey)

	c, err := HTTPClientMTLSFromFiles(caFile, certFile, keyFile, 5*time.Second)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	if _, err := KubernetesLogin(context.Background(), c, srv.URL, "kubernetes", "r", "j"); err != nil {
		t.Fatalf("initial login should succeed: %v", err)
	}

	// "Rotate" to a keypair from an untrusted CA, and drop pooled connections so
	// the next request performs a fresh handshake (a reused connection would
	// legitimately skip GetClientCertificate and mask the difference).
	_, otherCACert, otherCAKey := testCA(t, "untrusted-ca")
	_, badCert, badKey := issuePEM(t, otherCACert, otherCAKey, "reconciler", false)
	write(badCert, badKey)
	c.Transport.(*http.Transport).CloseIdleConnections()

	if _, err := KubernetesLogin(context.Background(), c, srv.URL, "kubernetes", "r", "j"); err == nil {
		t.Error("expected the rotated (untrusted) keypair to be re-read and rejected — the client is caching its certificate")
	}
}
