package keycloak

// Tests that followed the client here.
//
// A SHARED PACKAGE ACCUMULATES SYMBOLS FASTER THAN TESTS — that was the finding
// when four packages dipped below their coverage floors at once, and this package
// was born at risk of it: every method on Client arrived from somewhere else, and
// its tests stayed where the method used to live.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestKcDo_SetsContentTypeOnlyWithBody(t *testing.T) {
	type seen struct{ method, auth, ctype, body string }
	var got []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, seen{r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b)})
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

	resp, err := k.Do(http.MethodPost, "/with-body", map[string]any{"clientId": "llz"})
	if err != nil {
		t.Fatalf("POST with body: %v", err)
	}
	resp.Body.Close()
	resp, err = k.Do(http.MethodPut, "/no-body", nil)
	if err != nil {
		t.Fatalf("PUT without body: %v", err)
	}
	resp.Body.Close()

	if len(got) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(got))
	}
	if got[0].ctype != "application/json" {
		t.Errorf("body request Content-Type = %q, want application/json (Keycloak 415s a bare JSON write)", got[0].ctype)
	}
	if got[0].body != `{"clientId":"llz"}` {
		t.Errorf("body request sent %q, want the marshalled representation", got[0].body)
	}
	if got[1].ctype != "" {
		t.Errorf("bodyless request Content-Type = %q, want none", got[1].ctype)
	}
	if got[1].body != "" {
		t.Errorf("bodyless request sent a body %q", got[1].body)
	}
	for i, s := range got {
		if s.auth != "Bearer adm.tok" {
			t.Errorf("request %d Authorization = %q, want the admin bearer", i, s.auth)
		}
	}
}

// TestEnsureAudienceMapper_AcceptedStatuses pins the exact status set that counts
// as "the aud:llz mapper is in place": 201 (created) and 409 (already there).
// Anything else means OpenBao's bound_audiences will reject this client's tokens,
// so it MUST surface — a mapper check that accepts everything is the same as no
// mapper check at all.
func TestEnsureAudienceMapper_AcceptedStatuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"201 mapper created", http.StatusCreated, false},
		{"409 mapper already present (idempotent)", http.StatusConflict, false},
		{"400 bad mapper representation", http.StatusBadRequest, true},
		{"403 admin lacks manage-clients", http.StatusForbidden, true},
		{"500 keycloak error", http.StatusInternalServerError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/admin/realms/otomi/clients/cuuid/protocol-mappers/models" {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", http.StatusNotFound)
					return
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"errorMessage":"boom"}`))
			}))
			defer srv.Close()
			k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

			err := k.EnsureAudienceMapper("cuuid", DeviceClientID)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ensureAudienceMapper on HTTP %d = %v, wantErr=%v", tc.status, err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "audience mapper") {
				t.Errorf("error must name the failed step, got %v", err)
			}
			if body["protocolMapper"] != "oidc-audience-mapper" {
				t.Errorf("protocolMapper = %v, want oidc-audience-mapper", body["protocolMapper"])
			}
			cfg, _ := body["config"].(map[string]any)
			if cfg["included.client.audience"] != DeviceClientID {
				t.Errorf("mapper audience = %v, want %q (OpenBao bound_audiences)", cfg["included.client.audience"], DeviceClientID)
			}
		})
	}
}

// TestEnsureDeviceClient_PropagatesAudienceMapperFailure: the audience mapper is
// the last step of ensureDeviceClient, and a swallowed failure there is the worst
// case — the command prints "client ready" while every token it mints is rejected
// by OpenBao's bound_audiences.
func TestEnsureDeviceClient_PropagatesAudienceMapperFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case r.Method == http.MethodGet && p == "/admin/realms/otomi/clients":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "client-uuid"}})
		case r.Method == http.MethodGet && p == "/admin/realms/otomi/clients/client-uuid/default-client-scopes":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"name": "openid"}})
		case r.Method == http.MethodPost && p == "/admin/realms/otomi/clients/client-uuid/protocol-mappers/models":
			http.Error(w, "no manage-clients", http.StatusForbidden)
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

	uuid, err := k.EnsureDeviceClient("llz")
	if err == nil {
		t.Fatal("a failed audience mapper must fail ensureDeviceClient — otherwise the caller reports the client ready while OpenBao rejects its tokens")
	}
	if !strings.Contains(err.Error(), "audience mapper") {
		t.Errorf("error = %v, want it to name the audience mapper", err)
	}
	if uuid != "client-uuid" {
		t.Errorf("uuid = %q, want the client uuid returned alongside the error", uuid)
	}
}

// TestGetOrCreateClient_UsesLocationHeader: Keycloak returns the new client's id
// ONLY in the Location header. Ignoring it and re-querying is not equivalent — a
// stale/duplicate list answer would hand the caller the wrong client uuid, and
// every subsequent scope/mapper write would land on the wrong client.
func TestGetOrCreateClient_UsesLocationHeader(t *testing.T) {
	for _, tc := range []struct {
		name     string
		location string
		want     string
	}{
		{"Location header names the new client", "http://kc/admin/realms/otomi/clients/loc-uuid", "loc-uuid"},
		{"no Location header falls back to a re-query", "", "requery-uuid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch p := r.URL.Path; {
				case r.Method == http.MethodGet && p == "/admin/realms/otomi/clients":
					if posts == 0 { // not created yet
						_ = json.NewEncoder(w).Encode([]map[string]string{})
						return
					}
					_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "requery-uuid"}})
				case r.Method == http.MethodPost && p == "/admin/realms/otomi/clients":
					posts++
					if tc.location != "" {
						w.Header().Set("Location", tc.location)
					}
					w.WriteHeader(http.StatusCreated)
				default:
					t.Errorf("unexpected %s %s", r.Method, p)
					http.Error(w, "unexpected", http.StatusNotFound)
				}
			}))
			defer srv.Close()
			k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

			got, err := k.GetOrCreateClient("llz")
			if err != nil {
				t.Fatalf("getOrCreateClient: %v", err)
			}
			if got != tc.want {
				t.Errorf("client uuid = %q, want %q", got, tc.want)
			}
			if posts != 1 {
				t.Errorf("created the client %d times, want exactly 1", posts)
			}
		})
	}
}

// TestEnsureClientDefaultScope_AssignStatuses pins the accept set of the scope
// assignment (204/200). A rejected PUT that reads as success leaves the device
// client without the groups-carrying `openid` scope — `llz openbao login` then
// 403s forever while the bootstrap reports the client ready.
func TestEnsureClientDefaultScope_AssignStatuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"204 assigned", http.StatusNoContent, false},
		{"200 assigned", http.StatusOK, false},
		{"403 denied", http.StatusForbidden, true},
		{"404 scope vanished", http.StatusNotFound, true},
		{"500 keycloak error", http.StatusInternalServerError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var puts int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch p := r.URL.Path; {
				case r.Method == http.MethodGet && p == "/admin/realms/otomi/clients/cuuid/default-client-scopes":
					_ = json.NewEncoder(w).Encode([]map[string]string{{"name": "email"}}) // openid NOT yet assigned
				case r.Method == http.MethodGet && p == "/admin/realms/otomi/client-scopes":
					_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "sid-openid", "name": "openid"}})
				case r.Method == http.MethodPut && p == "/admin/realms/otomi/clients/cuuid/default-client-scopes/sid-openid":
					puts++
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"errorMessage":"boom"}`))
				default:
					t.Errorf("unexpected %s %s", r.Method, p)
					http.Error(w, "unexpected", http.StatusNotFound)
				}
			}))
			defer srv.Close()
			k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

			err := k.EnsureClientDefaultScope("cuuid", "openid")
			if (err != nil) != tc.wantErr {
				t.Fatalf("ensureClientDefaultScope on HTTP %d = %v, wantErr=%v", tc.status, err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "assign default scope openid") {
				t.Errorf("error must name the failed assignment, got %v", err)
			}
			if puts != 1 {
				t.Errorf("PUT count = %d, want 1", puts)
			}
		})
	}
}

// TestDecodeJSON_StatusBoundaries: decodeJSON is the gate in front of every admin
// GET, so its 2xx window has to be exactly [200,300). 300 (Multiple Choices) is
// NOT a success — a redirect body decoded as a client/scope list silently yields
// an empty list, which reads as "nothing provisioned yet" instead of an error.
func TestDecodeJSON_StatusBoundaries(t *testing.T) {
	for _, tc := range []struct {
		status  int
		wantErr bool
	}{
		{199, true},
		{200, false},
		{201, false},
		{299, false},
		{300, true}, // first non-success — the boundary that matters
		{301, true},
		{404, true},
		{500, true},
	} {
		t.Run(fmt.Sprintf("HTTP %d", tc.status), func(t *testing.T) {
			resp := &http.Response{
				StatusCode: tc.status,
				Body:       io.NopCloser(strings.NewReader(`[{"id":"sid-openid","name":"openid"}]`)),
			}
			var out []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			err := decodeJSON(resp, &out)
			if (err != nil) != tc.wantErr {
				t.Fatalf("decodeJSON on HTTP %d = %v, wantErr=%v", tc.status, err, tc.wantErr)
			}
			if tc.wantErr {
				if len(out) != 0 {
					t.Errorf("a non-2xx body must not be decoded, got %v", out)
				}
				return
			}
			if len(out) != 1 || out[0].Name != "openid" {
				t.Errorf("2xx body must decode, got %v", out)
			}
		})
	}
}

// TestWaitForClientScope_PollAndSleepBudget pins the ordering guard's budget: it
// polls EXACTLY ScopeAttempts times, sleeps between polls but not after
// the last one, and reports the real wall-clock budget it waited
// (attempts × interval) so an operator knows how long Keycloak was given.
func TestWaitForClientScope_PollAndSleepBudget(t *testing.T) {
	f := &fakeKeycloak{openidMissing: true} // apl-core never converges the scope
	srv := f.server(t)
	defer srv.Close()
	k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

	defer withScopeBudget(4, 100*time.Millisecond)()
	var sleeps []time.Duration
	err := k.WaitForClientScope("openid", func(d time.Duration) { sleeps = append(sleeps, d) })
	if err == nil {
		t.Fatal("a scope that never appears must time out")
	}
	if f.scopeGETs != 4 {
		t.Errorf("polled %d times, want exactly ScopeAttempts (4)", f.scopeGETs)
	}
	if len(sleeps) != 3 {
		t.Errorf("slept %d times, want attempts-1 (3) — no sleep after the final poll", len(sleeps))
	}
	for i, d := range sleeps {
		if d != 100*time.Millisecond {
			t.Errorf("sleep %d = %s, want ScopeInterval", i, d)
		}
	}
	// The message quotes attempts × interval — the budget actually spent.
	if !strings.Contains(err.Error(), "did not appear after ~400ms") {
		t.Errorf("timeout message must quote the real budget (4 × 100ms), got %v", err)
	}
}

// TestWaitForClientScope_StopsOnFirstSight: the counterpart budget check — once
// the scope exists the guard returns immediately, without burning a sleep.
func TestWaitForClientScope_StopsOnFirstSight(t *testing.T) {
	f := &fakeKeycloak{openidReadyAfter: 1} // present on the very first poll
	srv := f.server(t)
	defer srv.Close()
	k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

	defer withScopeBudget(4, 100*time.Millisecond)()
	var sleeps int
	if err := k.WaitForClientScope("openid", func(time.Duration) { sleeps++ }); err != nil {
		t.Fatalf("scope present on the first poll: %v", err)
	}
	if f.scopeGETs != 1 || sleeps != 0 {
		t.Errorf("polls=%d sleeps=%d, want 1 poll and no sleep", f.scopeGETs, sleeps)
	}
}

// TestKeycloakConnect_AttemptAndSleepBudget pins the readiness retry the same
// way: exactly ScopeAttempts port-forward+token attempts, with a sleep
// between attempts but not after the last one.
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

	tok, err := AdminToken(srv.Client(), srv.URL, "admin", "s3cret")
	if err != nil || tok != "adm.tok" {
		t.Fatalf("admin token = (%q, %v), want adm.tok", tok, err)
	}
	if _, err := AdminToken(srv.Client(), srv.URL, "admin", "wrong"); err == nil {
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

func TestEnsureDeviceClient_CreatesThenIdempotent(t *testing.T) {
	f := &fakeKeycloak{}
	srv := f.server(t)
	defer srv.Close()
	k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

	uuid, err := k.EnsureDeviceClient("llz")
	if err != nil || uuid != "client-uuid" {
		t.Fatalf("ensureDeviceClient = (%q, %v), want client-uuid", uuid, err)
	}
	// Second run must NOT create again (client now exists).
	if _, err := k.EnsureDeviceClient("llz"); err != nil {
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
	k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

	if _, err := k.EnsureDeviceClient("llz"); err != nil {
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
	k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

	_, err := k.EnsureDeviceClient("llz")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing openid scope must surface an actionable error, got %v", err)
	}
}

func TestWaitForClientScope_AppearsAfterRetries(t *testing.T) {
	f := &fakeKeycloak{openidReadyAfter: 3} // openid shows up on the 3rd poll
	srv := f.server(t)
	defer srv.Close()
	k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

	defer withScopeWait(5)()
	if err := k.WaitForClientScope("openid", func(time.Duration) {}); err != nil {
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
	k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

	defer withScopeWait(3)()
	err := k.WaitForClientScope("openid", func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "did not appear") {
		t.Errorf("missing scope must time out with an actionable error, got %v", err)
	}
}

func TestEnsureDirectGrantClient_PropagatesAudienceMapperFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case r.Method == http.MethodPost && p == "/admin/realms/otomi/clients":
			w.Header().Set("Location", "http://x/admin/realms/otomi/clients/cuuid")
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && p == "/admin/realms/otomi/clients/cuuid/default-client-scopes":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"name": "openid"}})
		case r.Method == http.MethodPost && p == "/admin/realms/otomi/clients/cuuid/protocol-mappers/models":
			http.Error(w, "no manage-clients", http.StatusForbidden)
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

	uuid, err := k.EnsureDirectGrantClient("llz-smoke-x")
	if err == nil {
		t.Fatal("a failed audience mapper must fail ensureDirectGrantClient — its tokens would not satisfy bound_audiences")
	}
	if !strings.Contains(err.Error(), "audience mapper") {
		t.Errorf("error = %v, want it to name the audience mapper", err)
	}
	if uuid != "cuuid" {
		t.Errorf("uuid = %q, want the created client's uuid returned alongside the error (teardown needs it)", uuid)
	}
}

// TestAddUserToGroup_AcceptedStatuses: group membership is what puts team-<name>
// in the groups claim. A membership PUT that failed but read as success turns
// into a confusing "token has no groups" failure two steps later.
func TestAddUserToGroup_AcceptedStatuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"204 joined", http.StatusNoContent, false},
		{"200 joined", http.StatusOK, false},
		{"403 denied", http.StatusForbidden, true},
		{"404 group or user gone", http.StatusNotFound, true},
		{"500 keycloak error", http.StatusInternalServerError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := kcStatusServer(t, http.MethodPut, "/admin/realms/otomi/users/uid-1/groups/gid-1", tc.status)
			defer srv.Close()
			k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

			err := k.AddUserToGroup("uid-1", "gid-1")
			if (err != nil) != tc.wantErr {
				t.Fatalf("addUserToGroup on HTTP %d = %v, wantErr=%v", tc.status, err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "add user to group") {
				t.Errorf("error must name the failed step, got %v", err)
			}
			if *hits != 1 {
				t.Errorf("PUT count = %d, want 1", *hits)
			}
		})
	}
}

// TestDeleteUser_AcceptedStatuses: teardown must not report clean on a failed
// delete — that leaves a real, enabled team-member user in the realm. 404 IS
// clean (already gone); a 403/500 is not.
func TestDeleteUser_AcceptedStatuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"204 deleted", http.StatusNoContent, false},
		{"200 deleted", http.StatusOK, false},
		{"404 already gone", http.StatusNotFound, false},
		{"403 denied", http.StatusForbidden, true},
		{"500 keycloak error", http.StatusInternalServerError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := kcStatusServer(t, http.MethodDelete, "/admin/realms/otomi/users/uid-1", tc.status)
			defer srv.Close()
			k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

			err := k.DeleteUser("uid-1")
			if (err != nil) != tc.wantErr {
				t.Fatalf("deleteUser on HTTP %d = %v, wantErr=%v", tc.status, err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "delete user uid-1") {
				t.Errorf("error must name the leaked user, got %v", err)
			}
			if *hits != 1 {
				t.Errorf("DELETE count = %d, want 1", *hits)
			}
		})
	}
}

// TestDisableUser_AcceptedStatuses: the teardown belt. If disabling silently
// "succeeds" while failing, an orphan smoke user keeps its password grant.
func TestDisableUser_AcceptedStatuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"204 disabled", http.StatusNoContent, false},
		{"200 disabled", http.StatusOK, false},
		{"404 already gone", http.StatusNotFound, true},
		{"500 keycloak error", http.StatusInternalServerError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := kcStatusServer(t, http.MethodPut, "/admin/realms/otomi/users/uid-1", tc.status)
			defer srv.Close()
			k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

			err := k.DisableUser("uid-1")
			if (err != nil) != tc.wantErr {
				t.Fatalf("disableUser on HTTP %d = %v, wantErr=%v", tc.status, err, tc.wantErr)
			}
			if *hits != 1 {
				t.Errorf("PUT count = %d, want 1", *hits)
			}
		})
	}
}

// TestDeleteClient_AcceptedStatuses: a leaked smoke client is a PUBLIC,
// ROPC-enabled client stamped aud:llz — a standing password-grant path into the
// OpenBao mount. A silently-failed delete must not look clean. 404 is clean.
func TestDeleteClient_AcceptedStatuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"204 deleted", http.StatusNoContent, false},
		{"200 deleted", http.StatusOK, false},
		{"404 already gone", http.StatusNotFound, false},
		{"403 denied", http.StatusForbidden, true},
		{"500 keycloak error", http.StatusInternalServerError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := kcStatusServer(t, http.MethodDelete, "/admin/realms/otomi/clients/cuuid", tc.status)
			defer srv.Close()
			k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

			err := k.DeleteClient("cuuid")
			if (err != nil) != tc.wantErr {
				t.Fatalf("deleteClient on HTTP %d = %v, wantErr=%v", tc.status, err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "delete client cuuid") {
				t.Errorf("error must name the leaked client, got %v", err)
			}
			if *hits != 1 {
				t.Errorf("DELETE count = %d, want 1", *hits)
			}
		})
	}
}
func TestRealmRoleExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/realms/otomi/roles/platform-admin":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "role-1", "name": "platform-admin"})
		case "/admin/realms/otomi/roles/team-missing":
			http.Error(w, "not found", http.StatusNotFound)
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	k := &Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

	if ok, err := k.RealmRoleExists("platform-admin"); err != nil || !ok {
		t.Errorf("realmRoleExists(platform-admin) = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := k.RealmRoleExists("team-missing"); err != nil || ok {
		t.Errorf("realmRoleExists(team-missing) = (%v, %v), want (false, nil)", ok, err)
	}
}

// withScopeBudget shrinks BOTH poll knobs, so a wait test runs instantly and the
// "~<duration>" in the give-up message is exactly predictable. A COPY of package
// main's — and the reason ScopeAttempts/ScopeInterval had to be exported vars
// rather than move here as consts.
func withScopeBudget(attempts int, interval time.Duration) func() {
	oldA, oldI := ScopeAttempts, ScopeInterval
	ScopeAttempts, ScopeInterval = attempts, interval
	return func() { ScopeAttempts, ScopeInterval = oldA, oldI }
}

func srvBase(r *http.Request) string { return "http://" + r.Host }

func withScopeWait(attempts int) func() {
	old := ScopeAttempts
	ScopeAttempts = attempts
	return func() { ScopeAttempts = old }
}

// TestKeycloakConnect_FailsFastOnAuthDenied: a 401 from the token endpoint is a
// permanent credential failure (wrong/disabled admin), so keycloakConnect returns
// immediately with an actionable error instead of retrying it as a not-ready
// timeout that would mask the real problem.
func countPrefix(ss []string, prefix string) int {
	n := 0
	for _, s := range ss {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

// kcStatusServer answers exactly one method+path with `status` (plus a short
// error body), counting hits; every other request fails the test. It lets a
// status table drive one helper's accept/reject decision with no network.
func kcStatusServer(t *testing.T, method, path string, status int) (*httptest.Server, *int) {
	t.Helper()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method || r.URL.Path != path {
			t.Errorf("unexpected %s %s (want %s %s)", r.Method, r.URL.Path, method, path)
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		hits++
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"errorMessage":"boom"}`))
	}))
	return srv, &hits
}
