package kube

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// newTestClient wires a Client at an httptest server that records the last
// request (method, path, content type, body) and replies per the handler.
func newTestServer(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "test-token", srv.Client()), srv
}

func TestGetJSON(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/api/v1/nodes/n1":
			w.Write([]byte(`{"spec":{"providerID":"linode://42"}}`))
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	})

	obj, status, err := c.GetJSON(context.Background(), "/api/v1/nodes/n1")
	if err != nil || status != 200 {
		t.Fatalf("GetJSON = (%d,%v)", status, err)
	}
	spec, _ := obj["spec"].(map[string]any)
	if spec["providerID"] != "linode://42" {
		t.Errorf("providerID = %v", spec["providerID"])
	}

	obj, status, err = c.GetJSON(context.Background(), "/missing")
	if err != nil || status != 404 || obj != nil {
		t.Errorf("404 should be (nil,404,nil), got (%v,%d,%v)", obj, status, err)
	}

	if _, _, err := c.GetJSON(context.Background(), "/forbidden"); err == nil {
		t.Error("non-2xx-non-404 should error")
	}
}

func TestCreateJSONAndConflict(t *testing.T) {
	var gotBody []byte
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("method/content-type = %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		gotBody, _ = io.ReadAll(r.Body)
		if r.URL.Path == "/conflict" {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})

	status, err := c.CreateJSON(context.Background(), "/api/v1/namespaces/kube-system/configmaps",
		map[string]any{"kind": "ConfigMap"})
	if err != nil || status != http.StatusCreated {
		t.Fatalf("CreateJSON = (%d,%v)", status, err)
	}
	var sent map[string]any
	if json.Unmarshal(gotBody, &sent); sent["kind"] != "ConfigMap" {
		t.Errorf("sent body = %s", gotBody)
	}

	// AlreadyExists is not an error — the object is there, which is the goal.
	if status, err := c.CreateJSON(context.Background(), "/conflict", map[string]any{}); err != nil || status != http.StatusConflict {
		t.Errorf("conflict = (%d,%v), want (409,nil)", status, err)
	}
}

func TestMergePatch(t *testing.T) {
	var gotCT string
	var gotBody []byte
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte("bad patch"))
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	err := c.MergePatch(context.Background(), "/api/v1/namespaces/kube-system/configmaps/x",
		map[string]any{"data": map[string]string{"K": "v"}})
	if err != nil {
		t.Fatalf("MergePatch: %v", err)
	}
	if gotCT != "application/merge-patch+json" {
		t.Errorf("content type = %q", gotCT)
	}
	if string(gotBody) != `{"data":{"K":"v"}}` {
		t.Errorf("body = %s", gotBody)
	}

	if err := c.MergePatch(context.Background(), "/fail", map[string]any{}); err == nil {
		t.Error("non-2xx should error")
	}
}

func TestNewInClusterRequiresEnv(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if _, err := NewInCluster(); err == nil {
		t.Error("NewInCluster outside a pod should error")
	}
}

func TestNewInCluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")

	dir := t.TempDir()
	tokenFile := dir + "/token"
	caFile := dir + "/ca.crt"
	if err := os.WriteFile(tokenFile, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caFile, selfSignedCAPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SA_TOKEN_FILE", tokenFile)
	t.Setenv("SA_CA_FILE", caFile)

	c, err := NewInCluster()
	if err != nil {
		t.Fatalf("NewInCluster: %v", err)
	}
	if c.base != "https://10.0.0.1:443" {
		t.Errorf("base = %q", c.base)
	}
	if c.token != "sa-token" { // trailing newline trimmed
		t.Errorf("token = %q", c.token)
	}

	// Missing token file.
	t.Setenv("SA_TOKEN_FILE", dir+"/absent")
	if _, err := NewInCluster(); err == nil {
		t.Error("missing token file should error")
	}
	t.Setenv("SA_TOKEN_FILE", tokenFile)

	// Unparseable CA bundle.
	badCA := dir + "/bad.crt"
	if err := os.WriteFile(badCA, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SA_CA_FILE", badCA)
	if _, err := NewInCluster(); err == nil {
		t.Error("unusable CA should error")
	}
	t.Setenv("SA_CA_FILE", dir+"/absent")
	if _, err := NewInCluster(); err == nil {
		t.Error("missing CA file should error")
	}
}

// selfSignedCAPEM mints a throwaway CA certificate for NewInCluster's CA-pool
// parse path.
func selfSignedCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
