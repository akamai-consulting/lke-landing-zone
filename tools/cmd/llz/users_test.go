package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/apl/identity"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/keycloak"
)

func TestRunUsersAdd_Guards(t *testing.T) {
	if err := runUsersAdd(globalOpts{}, usersAddOpts{}); err == nil {
		t.Error("missing --email must error")
	}
	if err := runUsersAdd(globalOpts{}, usersAddOpts{email: "a@b.c"}); err == nil {
		t.Error("no --team/--admin must error")
	}
	// Dry-run with valid intent is a clean no-op that never touches the cluster.
	if err := runUsersAdd(globalOpts{dryRun: true}, usersAddOpts{email: "a@b.c", admin: true}); err != nil {
		t.Errorf("dry-run must be a clean no-op, got %v", err)
	}
	// Without --yes (and not dry-run) is also plan-only — no cluster access.
	if err := runUsersAdd(globalOpts{}, usersAddOpts{email: "a@b.c", teams: []string{"platform"}}); err != nil {
		t.Errorf("plan-only (no --yes) must be a clean no-op, got %v", err)
	}
}

// ── kcClient user/role REST methods over a fake Keycloak admin API ────────────
//
// These exercise the HTTP transport that backs identity.AdminAPI. The onboarding
// orchestration itself is tested against an in-memory fake in internal/apl/identity.

type fakeUserKC struct {
	t             *testing.T
	usersByName   map[string]string // username -> id
	forceConflict bool              // force a 409 on the next create

	createdBody map[string]any // last POSTed user representation
}

func (f *fakeUserKC) server() *httptest.Server {
	realm := "/admin/realms/otomi"
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer adm.tok" {
			http.Error(w, "no bearer", http.StatusUnauthorized)
			return
		}
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && p == realm+"/roles/team-platform":
			_ = json.NewEncoder(w).Encode(identity.Role{ID: "role-team-platform", Name: "team-platform"})

		case r.Method == http.MethodGet && p == realm+"/roles/team-missing":
			http.Error(w, "no role", http.StatusNotFound)

		case r.Method == http.MethodPost && p == realm+"/users":
			_ = json.NewDecoder(r.Body).Decode(&f.createdBody)
			username, _ := f.createdBody["username"].(string)
			if f.forceConflict || f.usersByName[username] != "" {
				if f.usersByName[username] == "" {
					f.usersByName[username] = "user-existing"
				}
				http.Error(w, "exists", http.StatusConflict)
				return
			}
			id := "user-new"
			f.usersByName[username] = id
			w.Header().Set("Location", "http://"+r.Host+realm+"/users/"+id)
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodGet && p == realm+"/users":
			u := r.URL.Query().Get("username")
			var out []map[string]string
			if id := f.usersByName[u]; id != "" {
				out = append(out, map[string]string{"id": id, "username": u})
			}
			_ = json.NewEncoder(w).Encode(out)

		default:
			f.t.Errorf("unexpected %s %s?%s", r.Method, p, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
}

func newFakeKCClient(t *testing.T, f *fakeUserKC) (*keycloak.Client, func()) {
	f.t = t
	if f.usersByName == nil {
		f.usersByName = map[string]string{}
	}
	srv := f.server()
	return &keycloak.Client{HC: srv.Client(), Base: srv.URL, Token: "adm.tok", Realm: "otomi"}, srv.Close
}

func TestFindRealmRole(t *testing.T) {
	f := &fakeUserKC{}
	k, done := newFakeKCClient(t, f)
	defer done()

	rep, err := k.FindRealmRole("team-platform")
	if err != nil || rep == nil || rep.Name != "team-platform" {
		t.Fatalf("findRealmRole(existing) = (%v, %v), want the role", rep, err)
	}
	rep, err = k.FindRealmRole("team-missing")
	if err != nil || rep != nil {
		t.Fatalf("findRealmRole(missing) = (%v, %v), want (nil, nil)", rep, err)
	}
}

func TestEnsureUser_CreateAndConflict(t *testing.T) {
	f := &fakeUserKC{}
	k, done := newFakeKCClient(t, f)
	defer done()

	uid, created, err := k.EnsureUser(identity.UserRep{Username: "alice@corp.com", Enabled: true})
	if err != nil || !created || uid != "user-new" {
		t.Fatalf("create = (%q, %v, %v), want (user-new, true, nil)", uid, created, err)
	}
	// A second create for the same username 409s → found via lookup, created=false.
	uid, created, err = k.EnsureUser(identity.UserRep{Username: "alice@corp.com", Enabled: true})
	if err != nil || created || uid == "" {
		t.Fatalf("conflict = (%q, %v, %v), want (existing id, false, nil)", uid, created, err)
	}
}

func TestConsoleURLFor(t *testing.T) {
	if got := consoleURLFor(""); got != "" {
		t.Errorf("no region → %q, want empty", got)
	}
	dir := t.TempDir()
	writeResolveSpec(t, dir, "us-ord", "apps.example.com")
	t.Chdir(dir)
	if got := consoleURLFor("us-ord"); got != "https://console.apps.example.com" {
		t.Errorf("consoleURLFor = %q, want https://console.apps.example.com", got)
	}
	if got := consoleURLFor("nope"); got != "" {
		t.Errorf("unknown region → %q, want empty", got)
	}
}

func TestWarnUndeclaredTeams(t *testing.T) {
	// No spec / empty input must be a silent no-op (best-effort hint only).
	warnUndeclaredTeams(nil)
	dir := t.TempDir()
	writeResolveSpec(t, dir, "us-ord", "apps.example.com") // spec with no teams
	t.Chdir(dir)
	warnUndeclaredTeams([]string{"platform"}) // undeclared — warns to stderr, must not panic/err
}
