package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDesiredRoles(t *testing.T) {
	tests := []struct {
		name    string
		o       usersAddOpts
		want    []string
		wantErr bool
	}{
		{"teams", usersAddOpts{teams: []string{"platform", "gsap"}}, []string{"team-platform", "team-gsap"}, false},
		{"admin", usersAddOpts{admin: true}, []string{"team-admin"}, false},
		{"team+admin", usersAddOpts{teams: []string{"platform"}, admin: true}, []string{"team-platform", "team-admin"}, false},
		{"dedupe + trim", usersAddOpts{teams: []string{"platform", " platform ", ""}}, []string{"team-platform"}, false},
		{"admin dupe of team-admin", usersAddOpts{teams: []string{"admin"}, admin: true}, []string{"team-admin"}, false},
		{"none", usersAddOpts{}, nil, true},
		{"blank teams only", usersAddOpts{teams: []string{"", "  "}}, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := desiredRoles(tc.o)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && !equalStrings(got, tc.want) {
				t.Errorf("roles = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRandomPassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		pw := randomPassword()
		if len(pw) != pwLen {
			t.Fatalf("len(pw) = %d, want %d", len(pw), pwLen)
		}
		if !strings.ContainsAny(pw, pwLower) || !strings.ContainsAny(pw, pwUpper) ||
			!strings.ContainsAny(pw, pwDigit) || !strings.ContainsAny(pw, pwSpecial) {
			t.Fatalf("password %q missing a required character class", pw)
		}
		if seen[pw] {
			t.Fatalf("duplicate password %q across draws — not random", pw)
		}
		seen[pw] = true
	}
}

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

// ── fake Keycloak admin API for the user/role helpers ────────────────────────

type fakeUserKC struct {
	t             *testing.T
	roles         map[string]bool   // realm roles that exist
	groups        map[string]string // group name -> id (only provisioned ones)
	usersByName   map[string]string // username -> id
	forceConflict bool              // force a 409 on the next create

	createdBody   map[string]any // last POSTed user representation
	assignedRoles []string       // realm-role names assigned to the user
	groupAdds     []string       // "uid->gid" recorded
	emailedUsers  []string       // uids sent execute-actions-email
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
		case r.Method == http.MethodGet && strings.HasPrefix(p, realm+"/roles/"):
			name := strings.TrimPrefix(p, realm+"/roles/")
			if !f.roles[name] {
				http.Error(w, "no role", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(kcRole{ID: "role-" + name, Name: name})

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

		case r.Method == http.MethodPost && strings.HasSuffix(p, "/role-mappings/realm"):
			var roles []kcRole
			_ = json.NewDecoder(r.Body).Decode(&roles)
			for _, rr := range roles {
				f.assignedRoles = append(f.assignedRoles, rr.Name)
			}
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodGet && p == realm+"/groups":
			search := r.URL.Query().Get("search")
			var out []map[string]string
			if id := f.groups[search]; id != "" {
				out = append(out, map[string]string{"id": id, "name": search})
			}
			_ = json.NewEncoder(w).Encode(out)

		case r.Method == http.MethodPut && strings.Contains(p, "/groups/"):
			parts := strings.Split(p, "/")
			uid, gid := parts[len(parts)-3], parts[len(parts)-1]
			f.groupAdds = append(f.groupAdds, uid+"->"+gid)
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPut && strings.HasSuffix(p, "/execute-actions-email"):
			parts := strings.Split(p, "/")
			f.emailedUsers = append(f.emailedUsers, parts[len(parts)-2])
			w.WriteHeader(http.StatusNoContent)

		default:
			f.t.Errorf("unexpected %s %s?%s", r.Method, p, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
}

func newFakeKCClient(t *testing.T, f *fakeUserKC) (*kcClient, func()) {
	f.t = t
	if f.usersByName == nil {
		f.usersByName = map[string]string{}
	}
	srv := f.server()
	return &kcClient{hc: srv.Client(), base: srv.URL, token: "adm.tok", realm: "otomi"}, srv.Close
}

func TestFindRealmRole(t *testing.T) {
	f := &fakeUserKC{roles: map[string]bool{"team-platform": true}}
	k, done := newFakeKCClient(t, f)
	defer done()

	rep, err := k.findRealmRole("team-platform")
	if err != nil || rep == nil || rep.Name != "team-platform" {
		t.Fatalf("findRealmRole(existing) = (%v, %v), want the role", rep, err)
	}
	rep, err = k.findRealmRole("team-missing")
	if err != nil || rep != nil {
		t.Fatalf("findRealmRole(missing) = (%v, %v), want (nil, nil)", rep, err)
	}
}

func TestEnsureUser_CreateAndConflict(t *testing.T) {
	f := &fakeUserKC{}
	k, done := newFakeKCClient(t, f)
	defer done()

	uid, created, err := k.ensureUser(kcUserRep{Username: "alice@corp.com", Enabled: true})
	if err != nil || !created || uid != "user-new" {
		t.Fatalf("create = (%q, %v, %v), want (user-new, true, nil)", uid, created, err)
	}
	// A second create for the same username 409s → found via lookup, created=false.
	uid, created, err = k.ensureUser(kcUserRep{Username: "alice@corp.com", Enabled: true})
	if err != nil || created || uid == "" {
		t.Fatalf("conflict = (%q, %v, %v), want (existing id, false, nil)", uid, created, err)
	}
}

func TestApplyUserAdd_TempPasswordHappyPath(t *testing.T) {
	f := &fakeUserKC{
		roles:  map[string]bool{"team-platform": true, "team-admin": true},
		groups: map[string]string{"team-platform": "gid-platform"}, // team-admin group absent
	}
	k, done := newFakeKCClient(t, f)
	defer done()

	o := usersAddOpts{email: "alice@corp.com", teams: []string{"platform"}, admin: true}
	if err := applyUserAdd(k, o, o.email, []string{"team-platform", "team-admin"}); err != nil {
		t.Fatalf("applyUserAdd: %v", err)
	}
	// Created with a temporary credential + forced UPDATE_PASSWORD + verified email.
	if f.createdBody["emailVerified"] != true {
		t.Errorf("emailVerified = %v, want true (temp-password login)", f.createdBody["emailVerified"])
	}
	creds, _ := f.createdBody["credentials"].([]any)
	if len(creds) != 1 {
		t.Fatalf("want one inline credential, got %v", f.createdBody["credentials"])
	}
	cred, _ := creds[0].(map[string]any)
	if cred["temporary"] != true || cred["type"] != "password" || cred["value"] == "" {
		t.Errorf("credential = %v, want a temporary password", cred)
	}
	// Both roles granted; user added to the one existing group only.
	if !equalStrings(sortedCopy(f.assignedRoles), []string{"team-admin", "team-platform"}) {
		t.Errorf("assigned roles = %v, want both", f.assignedRoles)
	}
	if len(f.groupAdds) != 1 || !strings.HasSuffix(f.groupAdds[0], "->gid-platform") {
		t.Errorf("group adds = %v, want just the existing team-platform group", f.groupAdds)
	}
	if len(f.emailedUsers) != 0 {
		t.Errorf("temp-password path must NOT send email, got %v", f.emailedUsers)
	}
}

func TestApplyUserAdd_Invite(t *testing.T) {
	f := &fakeUserKC{roles: map[string]bool{"team-platform": true}}
	k, done := newFakeKCClient(t, f)
	defer done()

	o := usersAddOpts{email: "bob@corp.com", teams: []string{"platform"}, invite: true}
	if err := applyUserAdd(k, o, o.email, []string{"team-platform"}); err != nil {
		t.Fatalf("applyUserAdd: %v", err)
	}
	if f.createdBody["emailVerified"] != false {
		t.Errorf("invite path emailVerified = %v, want false", f.createdBody["emailVerified"])
	}
	if _, hasCreds := f.createdBody["credentials"]; hasCreds {
		t.Errorf("invite path must create the user password-less, got credentials %v", f.createdBody["credentials"])
	}
	if len(f.emailedUsers) != 1 {
		t.Errorf("invite path must send exactly one set-password email, got %v", f.emailedUsers)
	}
}

func TestApplyUserAdd_MissingRoleFailsBeforeCreate(t *testing.T) {
	f := &fakeUserKC{roles: map[string]bool{}} // team-platform NOT provisioned
	k, done := newFakeKCClient(t, f)
	defer done()

	err := applyUserAdd(k, usersAddOpts{email: "c@corp.com", teams: []string{"platform"}}, "c@corp.com", []string{"team-platform"})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing role must error before creating a user, got %v", err)
	}
	if f.createdBody != nil {
		t.Errorf("must not create a user when a role is missing, created %v", f.createdBody)
	}
}

func TestApplyUserAdd_ExistingUserAddOnly(t *testing.T) {
	f := &fakeUserKC{
		roles:         map[string]bool{"team-gsap": true},
		usersByName:   map[string]string{"dana@corp.com": "user-existing"},
		forceConflict: true,
	}
	k, done := newFakeKCClient(t, f)
	defer done()

	if err := applyUserAdd(k, usersAddOpts{email: "dana@corp.com", teams: []string{"gsap"}}, "dana@corp.com", []string{"team-gsap"}); err != nil {
		t.Fatalf("existing-user add-only: %v", err)
	}
	if !equalStrings(f.assignedRoles, []string{"team-gsap"}) {
		t.Errorf("assigned roles = %v, want [team-gsap] added to the existing user", f.assignedRoles)
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

// ── small test helpers ───────────────────────────────────────────────────────

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
