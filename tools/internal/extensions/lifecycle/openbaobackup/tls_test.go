package openbaobackup

import (
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

// The producer dials the chart's -active Service, whose name is NOT on OpenBao's
// serving certificate. It verifies against the plain Service name instead — the
// identity the certificate does prove. Pin both directions with a real handshake:
// pinned to the certificate's name it connects to an address the cert does not
// name; pinned to anything else it is refused. A client that "worked" by skipping
// verification would pass the first half and fail the second.
func TestProducerVerifiesTheServingCertByThePinnedServiceName(t *testing.T) {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "openbao-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)

	leaf := func(names []string, usage x509.ExtKeyUsage) ([]byte, []byte) {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "leaf"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			DNSNames: names, ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
		kb, _ := x509.MarshalECPrivateKey(k)
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	}
	// Serving cert: names the plain Service only — as openbao-tls does.
	srvCert, srvKey := leaf([]string{DefaultTLSServerName}, x509.ExtKeyUsageServerAuth)
	cliCert, cliKey := leaf(nil, x509.ExtKeyUsageClientAuth)

	dir := t.TempDir()
	for name, body := range map[string][]byte{
		"ca.crt":  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		"tls.crt": cliCert, "tls.key": cliKey,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OPENBAO_CA_FILE", filepath.Join(dir, "ca.crt"))
	t.Setenv("OPENBAO_CLIENT_CERT_FILE", filepath.Join(dir, "tls.crt"))
	t.Setenv("OPENBAO_CLIENT_KEY_FILE", filepath.Join(dir, "tls.key"))

	pair, err := tls.X509KeyPair(srvCert, srvKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()
	// Dial by IP — a name the certificate does not carry, like the -active Service.
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	url := "https://127.0.0.1:" + port + "/v1/sys/health"

	c, err := httpClientFn(DefaultTLSServerName)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("pinned to the certificate's own name, the handshake must verify: %v", err)
	}
	resp.Body.Close()

	c, err = httpClientFn("platform-openbao-active.llz-openbao.svc.cluster.local")
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := c.Get(url); err == nil {
		resp.Body.Close()
		t.Fatal("pinned to a name the certificate does not carry, the handshake was accepted — verification is not happening")
	}
}
