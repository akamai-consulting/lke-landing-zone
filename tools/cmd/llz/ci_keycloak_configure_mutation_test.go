package main

// ci_keycloak_configure_mutation_test.go — the guards in keycloak-configure that
// must FAIL a half-wired realm rather than report color.Green. Mutation testing found
// each of these inert-but-passing: a swallowed mapper error, a status set that
// accepts what it should reject (and rejects what it should accept), the
// created-client id read from the wrong place, and the poll/sleep budgets of the
// two retry loops. Every case is httptest/seam-driven — no network, no kubectl.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withScopeBudget shrinks BOTH poll knobs, so a wait test runs instantly and the
// "~<duration>" in the give-up message is exactly predictable.
func withScopeBudget(attempts int, interval time.Duration) func() {
	oldA, oldI := keycloakScopeAttempts, keycloakScopeInterval
	keycloakScopeAttempts, keycloakScopeInterval = attempts, interval
	return func() { keycloakScopeAttempts, keycloakScopeInterval = oldA, oldI }
}

// TestKcDo_SetsContentTypeOnlyWithBody: Keycloak rejects a JSON write that isn't
// declared application/json, and a bodyless GET/PUT must not claim one. The
// header therefore tracks the body, not the other way around.
func TestKcDo_SetsContentTypeOnlyWithBody(t *testing.T) {
	type seen struct{ method, auth, ctype, body string }
	var got []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, seen{r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b)})
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

	resp, err := k.do(http.MethodPost, "/with-body", map[string]any{"clientId": "llz"})
	if err != nil {
		t.Fatalf("POST with body: %v", err)
	}
	resp.Body.Close()
	resp, err = k.do(http.MethodPut, "/no-body", nil)
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
			k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

			err := k.ensureAudienceMapper("cuuid", keycloakDeviceClientID)
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
			if cfg["included.client.audience"] != keycloakDeviceClientID {
				t.Errorf("mapper audience = %v, want %q (OpenBao bound_audiences)", cfg["included.client.audience"], keycloakDeviceClientID)
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
	k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

	uuid, err := k.ensureDeviceClient("llz")
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
			k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

			got, err := k.getOrCreateClient("llz")
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
			k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

			err := k.ensureClientDefaultScope("cuuid", "openid")
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
// polls EXACTLY keycloakScopeAttempts times, sleeps between polls but not after
// the last one, and reports the real wall-clock budget it waited
// (attempts × interval) so an operator knows how long Keycloak was given.
func TestWaitForClientScope_PollAndSleepBudget(t *testing.T) {
	f := &fakeKeycloak{openidMissing: true} // apl-core never converges the scope
	srv := f.server(t)
	defer srv.Close()
	k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

	defer withScopeBudget(4, 100*time.Millisecond)()
	var sleeps []time.Duration
	err := k.waitForClientScope("openid", func(d time.Duration) { sleeps = append(sleeps, d) })
	if err == nil {
		t.Fatal("a scope that never appears must time out")
	}
	if f.scopeGETs != 4 {
		t.Errorf("polled %d times, want exactly keycloakScopeAttempts (4)", f.scopeGETs)
	}
	if len(sleeps) != 3 {
		t.Errorf("slept %d times, want attempts-1 (3) — no sleep after the final poll", len(sleeps))
	}
	for i, d := range sleeps {
		if d != 100*time.Millisecond {
			t.Errorf("sleep %d = %s, want keycloakScopeInterval", i, d)
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
	k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

	defer withScopeBudget(4, 100*time.Millisecond)()
	var sleeps int
	if err := k.waitForClientScope("openid", func(time.Duration) { sleeps++ }); err != nil {
		t.Fatalf("scope present on the first poll: %v", err)
	}
	if f.scopeGETs != 1 || sleeps != 0 {
		t.Errorf("polls=%d sleeps=%d, want 1 poll and no sleep", f.scopeGETs, sleeps)
	}
}

// TestKeycloakConnect_AttemptAndSleepBudget pins the readiness retry the same
// way: exactly keycloakScopeAttempts port-forward+token attempts, with a sleep
// between attempts but not after the last one.
func TestKeycloakConnect_AttemptAndSleepBudget(t *testing.T) {
	var attempts int
	orig := portForwardKeycloakFn
	portForwardKeycloakFn = func() (string, func(), error) {
		attempts++
		return "", func() {}, fmt.Errorf("pod not found")
	}
	defer func() { portForwardKeycloakFn = orig }()
	defer withScopeBudget(4, 100*time.Millisecond)()

	var sleeps []time.Duration
	_, _, cleanup, err := keycloakConnect(&http.Client{}, "u", "p", func(d time.Duration) { sleeps = append(sleeps, d) })
	cleanup()
	if err == nil {
		t.Fatal("a persistently-unreachable Keycloak must time out")
	}
	if attempts != 4 {
		t.Errorf("attempted %d times, want exactly keycloakScopeAttempts (4)", attempts)
	}
	if len(sleeps) != 3 {
		t.Errorf("slept %d times, want attempts-1 (3) — no sleep after the final attempt", len(sleeps))
	}
	for i, d := range sleeps {
		if d != 100*time.Millisecond {
			t.Errorf("sleep %d = %s, want keycloakScopeInterval", i, d)
		}
	}
}

// TestRunCIKeycloakConfigure_TeamsGate: spec.teams is the intent gate. No teams
// must short-circuit BEFORE any Keycloak work; teams declared must proceed to it.
// Both directions are checked from the printed output, in dry-run so nothing
// touches a cluster.
func TestRunCIKeycloakConfigure_TeamsGate(t *testing.T) {
	t.Run("no teams short-circuits", func(t *testing.T) {
		t.Chdir(t.TempDir()) // no landingzone.yaml → specTeams() is empty
		var err error
		var stdout string
		stderr := captureStderr(t, func() {
			stdout = captureStdout(t, func() { err = runCIKeycloakConfigure(globalOpts{dryRun: true}, "primary") })
		})
		if err != nil {
			t.Fatalf("no-teams run must be a clean no-op, got %v", err)
		}
		if !strings.Contains(stdout, "No spec.teams declared") {
			t.Errorf("stdout = %q, want the no-teams no-op message", stdout)
		}
		if strings.Contains(stderr, "dry-run") {
			t.Errorf("no teams must not reach the client work, stderr = %q", stderr)
		}
	})

	t.Run("teams declared proceeds to the client work", func(t *testing.T) {
		dir := t.TempDir()
		spec := `apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: t }
spec:
  instance: { upstreamOrg: akamai-consulting, repo: o/t, forge: github, templateVersion: v0.4.0 }
  teams:
    - { name: platform, openbaoSubtree: secret/platform }
  defaults:
    cluster:
      k8sVersion: v1.33.6+lke7
      nodePool: { type: g8-dedicated-8-4, count: 3 }
`
		if err := os.WriteFile(filepath.Join(dir, "landingzone.yaml"), []byte(spec), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		if teams := specTeams(); len(teams) != 1 {
			t.Fatalf("fixture spec must declare one team, got %v", teams)
		}
		var err error
		var stdout string
		stderr := captureStderr(t, func() {
			stdout = captureStdout(t, func() { err = runCIKeycloakConfigure(globalOpts{dryRun: true}, "primary") })
		})
		if err != nil {
			t.Fatalf("dry-run with teams: %v", err)
		}
		if !strings.Contains(stderr, "would ensure the public device-flow client") {
			t.Errorf("declared teams must reach the client work, stderr = %q", stderr)
		}
		if strings.Contains(stdout, "No spec.teams declared") {
			t.Errorf("declared teams must not print the no-teams no-op, stdout = %q", stdout)
		}
	})
}
