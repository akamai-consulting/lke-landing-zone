package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestKeycloakAdminToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/realms/master/protocol/openid-connect/token" ||
			r.Form.Get("grant_type") != "password" || r.Form.Get("client_id") != "admin-cli" ||
			r.Form.Get("username") != "admin" || r.Form.Get("password") != "s3cret" {
			http.Error(w, "bad", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "adm.tok"})
	}))
	defer srv.Close()

	tok, err := keycloakAdminToken(srv.Client(), srv.URL, "admin", "s3cret")
	if err != nil || tok != "adm.tok" {
		t.Fatalf("admin token = (%q, %v), want adm.tok", tok, err)
	}
	if _, err := keycloakAdminToken(srv.Client(), srv.URL, "admin", "wrong"); err == nil {
		t.Error("bad password must error")
	}
}

// fakeKeycloak is a minimal admin-REST stand-in that records the client write +
// default-scope assignment so tests can assert ensureDeviceClient creates the
// right (public, device-flow) client, reconciles the `openid` scope even on an
// existing client, and is idempotent. It deliberately serves NO group or
// protocol-mapper endpoints — the lean design leaves those to apl-core, so a hit
// on them is a regression the default case flags. Client-create does NOT
// auto-assign default scopes (Keycloak only honors defaultClientScopes in the
// body if the scope pre-existed) — so the reconcile PUT is what must attach it.
type fakeKeycloak struct {
	clientExists     bool
	created          []string        // "POST <path>" / "PUT scope <name>" audit trail
	clientBody       map[string]any  // the last created-client representation
	defaultScopes    map[string]bool // default-client-scope NAMES assigned to the client
	openidMissing    bool            // simulate apl-core's `openid` scope never appearing
	openidReadyAfter int             // openid scope appears only from this GET /client-scopes onward
	scopeGETs        int             // GET /client-scopes counter (for the wait test)
}

func (f *fakeKeycloak) server(t *testing.T) *httptest.Server {
	if f.defaultScopes == nil {
		f.defaultScopes = map[string]bool{}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer adm.tok" {
			http.Error(w, "no bearer", http.StatusUnauthorized)
			return
		}
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && p == "/admin/realms/otomi/clients":
			if f.clientExists {
				_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "client-uuid"}})
			} else {
				_ = json.NewEncoder(w).Encode([]map[string]string{})
			}
		case r.Method == http.MethodPost && p == "/admin/realms/otomi/clients":
			_ = json.NewDecoder(r.Body).Decode(&f.clientBody)
			f.created = append(f.created, "POST clients")
			f.clientExists = true
			w.Header().Set("Location", srvBase(r)+"/admin/realms/otomi/clients/client-uuid")
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && p == "/admin/realms/otomi/clients/client-uuid/default-client-scopes":
			var out []map[string]string
			for name := range f.defaultScopes {
				out = append(out, map[string]string{"name": name})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && p == "/admin/realms/otomi/client-scopes":
			f.scopeGETs++
			out := []map[string]string{{"id": "sid-email", "name": "email"}}
			if !f.openidMissing && f.scopeGETs >= f.openidReadyAfter {
				out = append(out, map[string]string{"id": "sid-openid", "name": "openid"})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPut && p == "/admin/realms/otomi/clients/client-uuid/default-client-scopes/sid-openid":
			f.created = append(f.created, "PUT scope openid")
			f.defaultScopes["openid"] = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && p == "/admin/realms/otomi/clients/client-uuid/protocol-mappers/models":
			// The ONLY mapper the lean design adds: an oidc-audience mapper (aud:llz)
			// so OpenBao's bound_audiences accepts this client's tokens. It must NOT
			// add a groups mapper — apl-core owns the groups claim.
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			if m["protocolMapper"] != "oidc-audience-mapper" {
				t.Errorf("unexpected protocol mapper %v — only the audience mapper is allowed (apl-core owns groups)", m["protocolMapper"])
			}
			f.created = append(f.created, "POST audience-mapper")
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected %s %s (the lean design adds only the openid scope + audience mapper)", r.Method, p)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
}

func srvBase(r *http.Request) string { return "http://" + r.Host }

func TestEnsureDeviceClient_CreatesThenIdempotent(t *testing.T) {
	f := &fakeKeycloak{}
	srv := f.server(t)
	defer srv.Close()
	k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

	uuid, err := k.ensureDeviceClient("llz")
	if err != nil || uuid != "client-uuid" {
		t.Fatalf("ensureDeviceClient = (%q, %v), want client-uuid", uuid, err)
	}
	// Second run must NOT create again (client now exists).
	if _, err := k.ensureDeviceClient("llz"); err != nil {
		t.Fatal(err)
	}
	if got := countPrefix(f.created, "POST clients"); got != 1 {
		t.Errorf("client created %d times, want exactly 1 (idempotent)", got)
	}
	// The client must be public, device-flow-enabled, and carry the openid scope
	// (it inherits apl-core's groups claim from that scope — no mapper of our own).
	if f.clientBody["publicClient"] != true {
		t.Errorf("client must be public, got %v", f.clientBody["publicClient"])
	}
	attrs, _ := f.clientBody["attributes"].(map[string]any)
	if attrs["oauth2.device.authorization.grant.enabled"] != "true" {
		t.Errorf("client must enable the device grant, got %v", attrs)
	}
	scopes, _ := f.clientBody["defaultClientScopes"].([]any)
	hasOpenID := false
	for _, s := range scopes {
		if s == "openid" {
			hasOpenID = true
		}
	}
	if !hasOpenID {
		t.Errorf("client must default the openid scope (carries the groups claim), got %v", scopes)
	}
	// And the openid scope must actually be reconciled onto the client (the fake
	// does not auto-assign on create), so the id_token will carry `groups`.
	if !f.defaultScopes["openid"] {
		t.Errorf("openid default scope was not assigned to the client")
	}
}

// TestEnsureDeviceClient_ReconcilesScopeOnExistingClient covers the ordering bug:
// a client that already exists WITHOUT the openid scope (created before apl-core
// converged the scope) must have it attached on a later run, else login 403s.
func TestEnsureDeviceClient_ReconcilesScopeOnExistingClient(t *testing.T) {
	f := &fakeKeycloak{clientExists: true} // exists, but defaultScopes is empty
	srv := f.server(t)
	defer srv.Close()
	k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

	if _, err := k.ensureDeviceClient("llz"); err != nil {
		t.Fatal(err)
	}
	if !f.defaultScopes["openid"] {
		t.Error("existing client missing openid scope was not reconciled — login would 403")
	}
	if got := countPrefix(f.created, "POST clients"); got != 0 {
		t.Errorf("must not recreate an existing client, got %d POSTs", got)
	}
}

// TestEnsureDeviceClient_ScopeMissingWarns: if apl-core's openid scope doesn't
// exist yet, reconcile returns an actionable error (the caller warns, best-effort).
func TestEnsureDeviceClient_ScopeMissingWarns(t *testing.T) {
	f := &fakeKeycloak{clientExists: true, openidMissing: true}
	srv := f.server(t)
	defer srv.Close()
	k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

	_, err := k.ensureDeviceClient("llz")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing openid scope must surface an actionable error, got %v", err)
	}
}

func withScopeWait(attempts int) func() {
	old := keycloakScopeAttempts
	keycloakScopeAttempts = attempts
	return func() { keycloakScopeAttempts = old }
}

// TestWaitForClientScope_AppearsAfterRetries: the ordering guard polls until
// apl-core converges the `openid` scope, instead of racing ahead and wiring a
// scope-less client.
func TestWaitForClientScope_AppearsAfterRetries(t *testing.T) {
	f := &fakeKeycloak{openidReadyAfter: 3} // openid shows up on the 3rd poll
	srv := f.server(t)
	defer srv.Close()
	k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

	defer withScopeWait(5)()
	if err := k.waitForClientScope("openid", func(time.Duration) {}); err != nil {
		t.Fatalf("scope appeared on poll 3 but wait failed: %v", err)
	}
	if f.scopeGETs < 3 {
		t.Errorf("expected to poll until openid appeared, got %d GETs", f.scopeGETs)
	}
}

// TestWaitForClientScope_Timeout: if the scope never converges, the wait gives up
// with an actionable error (the caller warns + exits 0, best-effort).
func TestWaitForClientScope_Timeout(t *testing.T) {
	f := &fakeKeycloak{openidMissing: true}
	srv := f.server(t)
	defer srv.Close()
	k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

	defer withScopeWait(3)()
	err := k.waitForClientScope("openid", func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "did not appear") {
		t.Errorf("missing scope must time out with an actionable error, got %v", err)
	}
}

func TestRunCIKeycloakConfigure_Guards(t *testing.T) {
	if err := runCIKeycloakConfigure(globalOpts{}, ""); err == nil {
		t.Error("missing --region must error")
	}
	// No spec in the test cwd → specTeams() is empty → clean no-op (not a failure).
	if err := runCIKeycloakConfigure(globalOpts{}, "primary"); err != nil {
		t.Errorf("no-teams run must be a clean no-op, got %v", err)
	}
}

func countPrefix(ss []string, prefix string) int {
	n := 0
	for _, s := range ss {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}
