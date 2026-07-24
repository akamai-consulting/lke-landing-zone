package identity

import (
	"sort"
	"strings"
	"testing"
)

func TestDesiredRoles(t *testing.T) {
	tests := []struct {
		name    string
		teams   []string
		admin   bool
		want    []string
		wantErr bool
	}{
		{"teams", []string{"platform", "gsap"}, false, []string{"team-platform", "team-gsap"}, false},
		{"admin", nil, true, []string{"team-admin"}, false},
		{"team+admin", []string{"platform"}, true, []string{"team-platform", "team-admin"}, false},
		{"dedupe + trim", []string{"platform", " platform ", ""}, false, []string{"team-platform"}, false},
		{"admin dupe of team-admin", []string{"admin"}, true, []string{"team-admin"}, false},
		{"none", nil, false, nil, true},
		{"blank teams only", []string{"", "  "}, false, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DesiredRoles(tc.teams, tc.admin)
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
		pw := RandomPassword()
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

// ── in-memory fake AdminAPI ──────────────────────────────────────────────────

type fakeAdmin struct {
	roles         map[string]bool   // realm roles that exist
	groups        map[string]string // group name -> id (only provisioned ones)
	usersByName   map[string]string // username -> id
	forceConflict bool              // report the next EnsureUser as pre-existing

	createdRep    *UserRep // last user representation passed to EnsureUser
	assignedRoles []string // realm-role names assigned
	groupAdds     []string // "uid->gid" recorded
	emailed       []string // uids sent execute-actions-email
}

func (f *fakeAdmin) FindRealmRole(name string) (*Role, error) {
	if !f.roles[name] {
		return nil, nil
	}
	return &Role{ID: "role-" + name, Name: name}, nil
}

func (f *fakeAdmin) EnsureUser(rep UserRep) (string, bool, error) {
	cp := rep
	f.createdRep = &cp
	if f.forceConflict || f.usersByName[rep.Username] != "" {
		if f.usersByName[rep.Username] == "" {
			f.usersByName[rep.Username] = "user-existing"
		}
		return f.usersByName[rep.Username], false, nil
	}
	id := "user-new"
	f.usersByName[rep.Username] = id
	return id, true, nil
}

func (f *fakeAdmin) AssignRealmRoles(_ string, roles []Role) error {
	for _, r := range roles {
		f.assignedRoles = append(f.assignedRoles, r.Name)
	}
	return nil
}

func (f *fakeAdmin) FindGroupID(name string) (string, error) { return f.groups[name], nil }

func (f *fakeAdmin) AddUserToGroup(userID, groupID string) error {
	f.groupAdds = append(f.groupAdds, userID+"->"+groupID)
	return nil
}

func (f *fakeAdmin) SendUpdatePasswordEmail(userID string) error {
	f.emailed = append(f.emailed, userID)
	return nil
}

func newFake() *fakeAdmin { return &fakeAdmin{usersByName: map[string]string{}} }

func TestAddUser_TempPasswordHappyPath(t *testing.T) {
	f := newFake()
	f.roles = map[string]bool{"team-platform": true, "team-admin": true}
	f.groups = map[string]string{"team-platform": "gid-platform"} // team-admin group absent

	res, err := AddUser(f, AddRequest{
		Username: "alice@corp.com", Email: "alice@corp.com",
		Roles: []string{"team-platform", "team-admin"},
	})
	if err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	if !res.Created || res.TempPassword == "" {
		t.Errorf("result = %+v, want created with a temp password", res)
	}
	// Created with a temporary credential + forced UPDATE_PASSWORD + verified email.
	if !f.createdRep.EmailVerified {
		t.Errorf("emailVerified = false, want true (temp-password login)")
	}
	if len(f.createdRep.Credentials) != 1 || !f.createdRep.Credentials[0].Temporary ||
		f.createdRep.Credentials[0].Type != "password" || f.createdRep.Credentials[0].Value == "" {
		t.Errorf("credentials = %+v, want one temporary password", f.createdRep.Credentials)
	}
	// Both roles granted; user added to the one existing group only.
	if !equalStrings(sortedCopy(f.assignedRoles), []string{"team-admin", "team-platform"}) {
		t.Errorf("assigned roles = %v, want both", f.assignedRoles)
	}
	if len(f.groupAdds) != 1 || !strings.HasSuffix(f.groupAdds[0], "->gid-platform") {
		t.Errorf("group adds = %v, want just the existing team-platform group", f.groupAdds)
	}
	if len(f.emailed) != 0 {
		t.Errorf("temp-password path must NOT send email, got %v", f.emailed)
	}
}

func TestAddUser_Invite(t *testing.T) {
	f := newFake()
	f.roles = map[string]bool{"team-platform": true}

	res, err := AddUser(f, AddRequest{
		Username: "bob@corp.com", Email: "bob@corp.com",
		Roles: []string{"team-platform"}, Invite: true,
	})
	if err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	if res.TempPassword != "" {
		t.Errorf("invite path must not set a temp password, got %q", res.TempPassword)
	}
	if f.createdRep.EmailVerified {
		t.Errorf("invite path emailVerified = true, want false")
	}
	if len(f.createdRep.Credentials) != 0 {
		t.Errorf("invite path must create the user password-less, got %+v", f.createdRep.Credentials)
	}
	if len(f.emailed) != 1 {
		t.Errorf("invite path must send exactly one set-password email, got %v", f.emailed)
	}
}

func TestAddUser_MissingRoleFailsBeforeCreate(t *testing.T) {
	f := newFake() // team-platform NOT provisioned

	_, err := AddUser(f, AddRequest{
		Username: "c@corp.com", Email: "c@corp.com", Roles: []string{"team-platform"},
	})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing role must error before creating a user, got %v", err)
	}
	if f.createdRep != nil {
		t.Errorf("must not create a user when a role is missing, created %+v", f.createdRep)
	}
}

func TestAddUser_ExistingUserAddOnly(t *testing.T) {
	f := newFake()
	f.roles = map[string]bool{"team-gsap": true}
	f.usersByName = map[string]string{"dana@corp.com": "user-existing"}
	f.forceConflict = true

	res, err := AddUser(f, AddRequest{
		Username: "dana@corp.com", Email: "dana@corp.com", Roles: []string{"team-gsap"},
	})
	if err != nil {
		t.Fatalf("existing-user add-only: %v", err)
	}
	if res.Created || res.TempPassword != "" {
		t.Errorf("result = %+v, want not-created with no temp password", res)
	}
	if !equalStrings(f.assignedRoles, []string{"team-gsap"}) {
		t.Errorf("assigned roles = %v, want [team-gsap] added to the existing user", f.assignedRoles)
	}
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
	sort.Strings(out)
	return out
}
