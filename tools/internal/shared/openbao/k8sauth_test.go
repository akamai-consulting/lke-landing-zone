package openbao

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestKubernetesLogin(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"auth":{"client_token":"s.abc123"}}`))
	}))
	defer srv.Close()

	tok, err := KubernetesLogin(context.Background(), srv.Client(), srv.URL, "kubernetes", "linode-rotator", "jwt-xyz")
	if err != nil {
		t.Fatalf("KubernetesLogin: %v", err)
	}
	if tok != "s.abc123" {
		t.Errorf("token = %q, want s.abc123", tok)
	}
	if gotPath != "/v1/auth/kubernetes/login" {
		t.Errorf("login path = %q", gotPath)
	}
	if gotBody == "" || !contains(gotBody, `"role":"linode-rotator"`) || !contains(gotBody, `"jwt":"jwt-xyz"`) {
		t.Errorf("login body = %q, want role+jwt", gotBody)
	}
}

func TestKubernetesLoginErrors(t *testing.T) {
	// Non-2xx surfaces an error with the status.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
	}))
	defer bad.Close()
	if _, err := KubernetesLogin(context.Background(), bad.Client(), bad.URL, "kubernetes", "r", "j"); err == nil {
		t.Error("expected error on HTTP 403")
	}

	// 2xx with no client_token is an error too.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"auth":{}}`))
	}))
	defer empty.Close()
	if _, err := KubernetesLogin(context.Background(), empty.Client(), empty.URL, "kubernetes", "r", "j"); err == nil {
		t.Error("expected error when no client_token is returned")
	}
}

func TestHTTPClientWithCA(t *testing.T) {
	if _, err := HTTPClientWithCA([]byte("not a pem"), time.Second); err == nil {
		t.Error("expected error for an invalid CA bundle")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
