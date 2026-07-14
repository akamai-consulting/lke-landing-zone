package openbao

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPClientInsecure(t *testing.T) {
	c := HTTPClientInsecure(7 * time.Second)
	if c == nil {
		t.Fatal("HTTPClientInsecure returned nil")
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
