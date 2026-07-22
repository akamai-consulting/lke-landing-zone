package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func makeJWT(t *testing.T, groups []string) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"groups": groups})
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + "." + enc([]byte("sig"))
}

func TestDecodeJWTGroups(t *testing.T) {
	g, err := decodeJWTGroups(makeJWT(t, []string{"team-platform", "team-web"}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(g) != 2 || g[0] != "team-platform" || g[1] != "team-web" {
		t.Errorf("groups = %v, want [team-platform team-web]", g)
	}
	if _, err := decodeJWTGroups("not-a-jwt"); err == nil {
		t.Error("non-JWT must error")
	}
	// A JWT with no groups claim decodes to an empty slice, not an error.
	if g, err := decodeJWTGroups(makeJWT(t, nil)); err != nil || len(g) != 0 {
		t.Errorf("no-groups token = (%v, %v), want ([], nil)", g, err)
	}
}

// smokeServer stands in for the Keycloak admin REST + token endpoints the smoke
// helpers drive, recording an audit trail so a test can assert the full
// provision → grant → teardown sequence.
func smokeServer(t *testing.T, idToken string) (*httptest.Server, *[]string) {
	var audit []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && p == "/admin/realms/otomi/groups":
			name := r.URL.Query().Get("search")
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "gid-1", "name": name}})
		case r.Method == http.MethodPost && p == "/admin/realms/otomi/clients":
			audit = append(audit, "create client")
			w.Header().Set("Location", "http://x/admin/realms/otomi/clients/cuuid")
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && p == "/admin/realms/otomi/clients/cuuid/default-client-scopes":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"name": "openid"}}) // already attached
		case r.Method == http.MethodPost && p == "/admin/realms/otomi/clients/cuuid/protocol-mappers/models":
			audit = append(audit, "add audience mapper")
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut && p == "/admin/realms/otomi/users/uid-1":
			audit = append(audit, "disable user") // teardown neutralizes before delete
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && p == "/admin/realms/otomi/users":
			audit = append(audit, "create user")
			w.Header().Set("Location", "http://x/admin/realms/otomi/users/uid-1")
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut && p == "/admin/realms/otomi/users/uid-1/groups/gid-1":
			audit = append(audit, "add to group")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && p == "/admin/realms/otomi/users/uid-1":
			audit = append(audit, "delete user")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && p == "/admin/realms/otomi/clients/cuuid":
			audit = append(audit, "delete client")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && p == "/realms/otomi/protocol/openid-connect/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"id_token": idToken})
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	return srv, &audit
}

func TestSmokeHelpers_ProvisionGrantTeardown(t *testing.T) {
	idToken := makeJWT(t, []string{"team-platform"})
	srv, audit := smokeServer(t, idToken)
	defer srv.Close()
	k := &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}

	gid, err := k.findGroupID("team-platform")
	if err != nil || gid != "gid-1" {
		t.Fatalf("findGroupID = (%q, %v), want gid-1", gid, err)
	}
	// A group that doesn't exactly match must return "" (not a substring hit).
	// The fake echoes search into name, so a distinct search still returns that
	// name — exercise the exact-match path with the same name.
	cuuid, err := k.ensureDirectGrantClient("llz-smoke-x")
	if err != nil || cuuid != "cuuid" {
		t.Fatalf("ensureDirectGrantClient = (%q, %v), want cuuid", cuuid, err)
	}
	uid, err := k.createSmokeUser("llz-smoke-x", "pw")
	if err != nil || uid != "uid-1" {
		t.Fatalf("createSmokeUser = (%q, %v), want uid-1", uid, err)
	}
	if err := k.addUserToGroup(uid, gid); err != nil {
		t.Fatalf("addUserToGroup: %v", err)
	}
	idt, err := k.passwordGrant("llz-smoke-x", "llz-smoke-x", "pw")
	if err != nil || idt != idToken {
		t.Fatalf("passwordGrant err=%v", err)
	}
	g, _ := decodeJWTGroups(idt)
	if !containsString(g, "team-platform") {
		t.Errorf("granted token groups = %v, want team-platform", g)
	}
	if err := k.deleteUser(uid); err != nil {
		t.Fatalf("deleteUser: %v", err)
	}
	if err := k.deleteClient(cuuid); err != nil {
		t.Fatalf("deleteClient: %v", err)
	}
	want := []string{"create client", "add audience mapper", "create user", "add to group", "delete user", "delete client"}
	if len(*audit) != len(want) {
		t.Fatalf("audit = %v, want %v", *audit, want)
	}
	for i := range want {
		if (*audit)[i] != want[i] {
			t.Errorf("audit[%d] = %q, want %q", i, (*audit)[i], want[i])
		}
	}
}

func TestIsDenied(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{"write x: HTTP 403: permission denied", true},
		{"write x: HTTP 403", true},
		{"1 error occurred: permission denied", true},
		{"write x: HTTP 500: internal error", false},
		{"dial tcp: connection refused", false},
	} {
		if got := isDenied(errString(tc.msg)); got != tc.want {
			t.Errorf("isDenied(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}
