package main

// ci_keycloak_smoke_mutation_test.go — status handling in the smoke's realm
// writes and teardown. Mutation testing showed these checks could be made inert
// without a single test noticing: a failed teardown would then read as clean
// while a PUBLIC, ROPC-enabled, aud:llz-stamped client (or a live team-member
// user) stays standing in the realm. Each helper gets a table over its EXACT
// accepted status set plus a rejected one.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

// TestEnsureDirectGrantClient_PropagatesAudienceMapperFailure: the smoke client's
// tokens only satisfy OpenBao's bound_audiences because of the aud:llz mapper. If
// stamping it fails and the error is swallowed, the smoke goes on to blame the
// OpenBao role for the login failure the mapper caused.
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
	k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

	uuid, err := k.ensureDirectGrantClient("llz-smoke-x")
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
			k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

			err := k.addUserToGroup("uid-1", "gid-1")
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
			k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

			err := k.deleteUser("uid-1")
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
			k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

			err := k.disableUser("uid-1")
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
			k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

			err := k.deleteClient("cuuid")
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
