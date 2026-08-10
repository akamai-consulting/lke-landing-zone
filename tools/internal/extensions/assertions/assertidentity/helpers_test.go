package assertidentity

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/keycloak"
)

// errString is a string-valued error. A COPY — package main's comment already
// records that it was duplicated back there after the cluster-access extraction
// took its copy, and this is the same fixture crossing the same kind of boundary
// again. A test fixture cannot be imported.
type errString string

func (e errString) Error() string { return string(e) }

func certItemJSON(ns, name, ready, notAfter, reason string) string {
	return `{"metadata":{"name":"` + name + `","namespace":"` + ns + `"},
	  "status":{"notAfter":"` + notAfter + `","conditions":[{"type":"Ready","status":"` + ready + `","reason":"` + reason + `","message":"m"}]}}`
}

func certListJSON(items ...string) []byte {
	return []byte(`{"items":[` + strings.Join(items, ",") + `]}`)
}
func TestSmokeHelpers_ProvisionGrantTeardown(t *testing.T) {
	idToken := makeJWT(t, []string{"team-platform"})
	srv, audit := smokeServer(t, idToken)
	defer srv.Close()
	k := &keycloak.Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}

	gid, err := k.FindGroupID("team-platform")
	if err != nil || gid != "gid-1" {
		t.Fatalf("findGroupID = (%q, %v), want gid-1", gid, err)
	}
	// A group that doesn't exactly match must return "" (not a substring hit).
	// The fake echoes search into name, so a distinct search still returns that
	// name — exercise the exact-match path with the same name.
	cuuid, err := k.EnsureDirectGrantClient("llz-smoke-x")
	if err != nil || cuuid != "cuuid" {
		t.Fatalf("ensureDirectGrantClient = (%q, %v), want cuuid", cuuid, err)
	}
	uid, err := k.CreateSmokeUser("llz-smoke-x", "pw")
	if err != nil || uid != "uid-1" {
		t.Fatalf("createSmokeUser = (%q, %v), want uid-1", uid, err)
	}
	if err := k.AddUserToGroup(uid, gid); err != nil {
		t.Fatalf("addUserToGroup: %v", err)
	}
	idt, err := k.PasswordGrant("llz-smoke-x", "llz-smoke-x", "pw")
	if err != nil || idt != idToken {
		t.Fatalf("passwordGrant err=%v", err)
	}
	g, _ := decodeJWTGroups(idt)
	if !containsString(g, "team-platform") {
		t.Errorf("granted token groups = %v, want team-platform", g)
	}
	if err := k.DeleteUser(uid); err != nil {
		t.Fatalf("deleteUser: %v", err)
	}
	if err := k.DeleteClient(cuuid); err != nil {
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
