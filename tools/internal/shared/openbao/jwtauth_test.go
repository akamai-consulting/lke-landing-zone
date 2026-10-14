package openbao

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJWTLogin(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"auth":{"client_token":"s.jwt-token"}}`))
	}))
	defer srv.Close()

	tok, err := JWTLogin(context.Background(), srv.Client(), srv.URL, "platform-ci", "gh-oidc-jwt")
	if err != nil {
		t.Fatalf("JWTLogin: %v", err)
	}
	if tok != "s.jwt-token" {
		t.Errorf("token = %q, want s.jwt-token", tok)
	}
	if gotPath != "/v1/auth/jwt/login" {
		t.Errorf("login path = %q, want /v1/auth/jwt/login", gotPath)
	}
	if !contains(gotBody, `"role":"platform-ci"`) || !contains(gotBody, `"jwt":"gh-oidc-jwt"`) {
		t.Errorf("login body = %q, want role+jwt", gotBody)
	}
}

func TestJWTLoginErrors(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
	}))
	defer bad.Close()
	if _, err := JWTLogin(context.Background(), bad.Client(), bad.URL, "platform-ci", "j"); err == nil {
		t.Error("expected error on HTTP 403")
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"auth":{}}`))
	}))
	defer empty.Close()
	if _, err := JWTLogin(context.Background(), empty.Client(), empty.URL, "platform-ci", "j"); err == nil {
		t.Error("expected error when no client_token is returned")
	}
}
