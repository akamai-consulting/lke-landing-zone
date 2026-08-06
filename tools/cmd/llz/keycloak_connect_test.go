package main

// TestKeycloakConnect_* STAYED in package main.
//
// They drive portForwardKeycloakFn — the port-forward seam the CONFIGURE verb owns
// — not the admin client. My classifier moved them because their bodies mention
// AdminToken, which is the classifier's one blind spot: a test can reference a
// package's symbol while being about something else entirely. Worth stating,
// because the classify-then-split pass has otherwise been reliable across seven
// files.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/keycloak"
)

func TestKeycloakConnect_RetriesUntilServing(t *testing.T) {
	var tokenCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/realms/master/protocol/openid-connect/token") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		tokenCalls++
		if tokenCalls < 3 { // server not serving yet on the first two attempts
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"adm.tok"}`))
	}))
	defer srv.Close()

	orig := portForwardKeycloakFn
	portForwardKeycloakFn = func() (string, func(), error) { return srv.URL, func() {}, nil }
	defer func() { portForwardKeycloakFn = orig }()
	defer withScopeWait(5)()

	base, token, cleanup, err := keycloakConnect(srv.Client(), "u", "p", func(time.Duration) {})
	defer cleanup()
	if err != nil {
		t.Fatalf("connect should succeed once the server answers: %v", err)
	}
	if token != "adm.tok" || base != srv.URL {
		t.Errorf("got base=%q token=%q, want %q / adm.tok", base, token, srv.URL)
	}
	if tokenCalls < 3 {
		t.Errorf("expected retries until the server answered, got %d token calls", tokenCalls)
	}
}

// TestKeycloakConnect_Timeout: a persistently-unreachable Keycloak (port-forward
// never opens) times out with an actionable error — the caller then warns + exits 0.
func TestKeycloakConnect_FailsFastOnAuthDenied(t *testing.T) {
	var tokenCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/realms/master/protocol/openid-connect/token") {
			tokenCalls++
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	orig := portForwardKeycloakFn
	portForwardKeycloakFn = func() (string, func(), error) { return srv.URL, func() {}, nil }
	defer func() { portForwardKeycloakFn = orig }()
	defer withScopeWait(30)() // generous budget — the point is we DON'T consume it

	_, _, _, err := keycloakConnect(srv.Client(), "u", "p", func(time.Duration) {})
	if err == nil || !errors.Is(err, keycloak.ErrAuthDenied) {
		t.Fatalf("401 must fail fast with keycloak.ErrAuthDenied, got %v", err)
	}
	if tokenCalls != 1 {
		t.Errorf("auth-denied must not retry: got %d token calls, want 1", tokenCalls)
	}
}
