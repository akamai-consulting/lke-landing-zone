package identity

import (
	"errors"
	"strings"
	"testing"
)

// identity_mutation_test.go pins the two AddUser branches whose flip is invisible
// to the happy-path tests: WHICH hint a missing realm role gets (the
// rn == PlatformAdminRole comparison), and the group-mirroring error branch
// (warn only on a real failure, never on success).

// TestAddUser_MissingRoleHintIsRoleSpecific asserts the missing-role error carries
// the hint for the KIND of role that is missing: a team role points at spec.teams,
// platform-admin points at apl-core's built-in admin role. Flipping the comparison
// swaps the two hints and sends the operator to the wrong remedy.
func TestAddUser_MissingRoleHintIsRoleSpecific(t *testing.T) {
	const (
		teamHint  = "declare it in spec.teams"
		adminHint = "is this a converged APL cluster?"
	)
	cases := []struct {
		name     string
		role     string
		want     string
		wantNot  string
		wantRole string
	}{
		{"team role", "team-platform", teamHint, adminHint, "team-platform"},
		{"platform-admin", PlatformAdminRole, adminHint, teamHint, PlatformAdminRole},
		// A role merely CONTAINING the admin role's name is still a team role.
		{"lookalike team role", "team-" + PlatformAdminRole, teamHint, adminHint, "team-" + PlatformAdminRole},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake() // no realm roles provisioned at all
			_, err := AddUser(f, AddRequest{Username: "u@corp.com", Roles: []string{tc.role}})
			if err == nil {
				t.Fatalf("missing role %q must error", tc.role)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error for missing %q = %q, want it to contain %q", tc.role, msg, tc.want)
			}
			if strings.Contains(msg, tc.wantNot) {
				t.Errorf("error for missing %q = %q, must NOT contain the other role kind's hint %q", tc.role, msg, tc.wantNot)
			}
			if !strings.Contains(msg, `realm role "`+tc.wantRole+`" does not exist`) {
				t.Errorf("error = %q, want it to name the missing role %q", msg, tc.wantRole)
			}
		})
	}
}

// failingGroupAdd wraps the package fake so only AddUserToGroup fails — the
// non-fatal branch (the role is already granted, so a group-mirroring failure is
// reported as a warning, not an error).
type failingGroupAdd struct {
	*fakeAdmin
	err error
}

func (f failingGroupAdd) AddUserToGroup(string, string) error { return f.err }

// TestAddUser_GroupAddErrorWarnsOnlyOnFailure asserts the two sides of the
// AddUserToGroup error check: a successful add produces NO warning, and a failed
// add produces exactly one warning while AddUser still succeeds.
func TestAddUser_GroupAddErrorWarnsOnlyOnFailure(t *testing.T) {
	t.Run("success is silent", func(t *testing.T) {
		f := newFake()
		f.roles = map[string]bool{"team-platform": true}
		f.groups = map[string]string{"team-platform": "gid-platform"}

		res, err := AddUser(f, AddRequest{Username: "alice@corp.com", Roles: []string{"team-platform"}})
		if err != nil {
			t.Fatalf("AddUser: %v", err)
		}
		if len(f.groupAdds) != 1 {
			t.Fatalf("group adds = %v, want the one existing group added", f.groupAdds)
		}
		if len(res.GroupWarnings) != 0 {
			t.Errorf("a SUCCESSFUL group add must produce no warning, got %v", res.GroupWarnings)
		}
	})

	t.Run("failure warns but does not fail the add", func(t *testing.T) {
		inner := newFake()
		inner.roles = map[string]bool{"team-platform": true}
		inner.groups = map[string]string{"team-platform": "gid-platform"}
		f := failingGroupAdd{fakeAdmin: inner, err: errors.New("403 forbidden")}

		res, err := AddUser(f, AddRequest{Username: "alice@corp.com", Roles: []string{"team-platform"}})
		if err != nil {
			t.Fatalf("a group-mirroring failure must NOT fail the add: %v", err)
		}
		if len(res.GroupWarnings) != 1 {
			t.Fatalf("group warnings = %v, want exactly one", res.GroupWarnings)
		}
		w := res.GroupWarnings[0]
		if !strings.Contains(w, `could not add "alice@corp.com" to group "team-platform"`) ||
			!strings.Contains(w, "403 forbidden") {
			t.Errorf("warning = %q, want it to name the user, the group and the underlying error", w)
		}
		if !equalStrings(inner.assignedRoles, []string{"team-platform"}) {
			t.Errorf("assigned roles = %v, want the role granted regardless", inner.assignedRoles)
		}
	})
}
