package openbao

// inclusterclient_test.go — the memoizing in-cluster HTTP client, tested from
// inside the package.
//
// The constructor is EXPORTED (NewInClusterHTTPClient) rather than reached for
// through a test hook: a constructor plus a package-level default built from it is
// an ordinary Go pair, and the memo it builds is exactly the thing a caller with
// different TLS material would want to rebuild. Exporting it for the test alone
// would have been the tail wagging the dog; exporting it because it is the API is
// not.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInClusterBaoClientRetriesAfterFailure(t *testing.T) {
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	crt := filepath.Join(dir, "tls.crt")
	key := filepath.Join(dir, "tls.key")
	t.Setenv("OPENBAO_CA_FILE", ca)
	t.Setenv("OPENBAO_CLIENT_CERT_FILE", crt)
	t.Setenv("OPENBAO_CLIENT_KEY_FILE", key)

	get := NewInClusterHTTPClient()

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
