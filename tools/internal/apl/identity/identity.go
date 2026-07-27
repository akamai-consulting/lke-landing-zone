// Package identity is the APL user-onboarding domain, lifted out of package main
// per ADR 0002 Phase 1 (docs/adr/0002-llz-as-apl-cli.md). Given a Keycloak admin
// API, it validates the requested access, creates (or finds) the user, grants the
// team/admin realm roles, mirrors any already-provisioned group membership, and
// optionally emails a set-password invite.
//
// Transport and cluster access stay ABOVE this package: the caller supplies an
// AdminAPI (cmd/llz's kcClient over a kubectl port-forward). identity does no I/O
// and no printing — AddUser returns a Result the caller renders — so it is
// unit-testable against an in-memory fake AdminAPI. AdminAPI is to identity what
// provider.ClusterProvider is to the cluster: the substrate seam it depends on,
// declared here at the consumer.
package identity

import (
	"fmt"
	"strings"
)

// PlatformAdminRole is the realm role `--admin` grants: apl-core's built-in admin
// TEAM role in the otomi realm. It is not a spec.teams entry.
//
// NOT the same thing as apl-core's built-in all-teams admin role `platform-admin`,
// despite this name. The default admin accounts (`otomi-admin`,
// `platform-admin@<domain>`) carry `platform-admin` and are NOT in `team-admin` —
// verified live, and the reason cmd/llz's OpenBao keycloak roles bind
// `aplPlatformAdminRole` ("platform-admin") rather than this constant.
//
// Consequence, deliberately left standing for now: a user created with `--admin`
// cannot `llz openbao login --team <name>`, because its groups claim carries
// `team-admin`, which no keycloak role binds. Reconciling the two — most likely by
// granting `platform-admin` here — is a follow-up; until then `--team <name>` is
// the working grant. See docs/runbooks/openbao-team-login.md troubleshooting.
const PlatformAdminRole = "team-admin"

// Role is a Keycloak realm role (the subset role-mapping needs). JSON tags match
// the admin REST wire form so an AdminAPI implementation can decode straight into
// it.
type Role struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Credential is an inline password credential in a user representation.
type Credential struct {
	Type      string `json:"type"`
	Value     string `json:"value"`
	Temporary bool   `json:"temporary"`
}

// UserRep is the user representation POSTed to create a user.
type UserRep struct {
	Username        string       `json:"username"`
	Email           string       `json:"email,omitempty"`
	FirstName       string       `json:"firstName,omitempty"`
	LastName        string       `json:"lastName,omitempty"`
	Enabled         bool         `json:"enabled"`
	EmailVerified   bool         `json:"emailVerified"`
	RequiredActions []string     `json:"requiredActions,omitempty"`
	Credentials     []Credential `json:"credentials,omitempty"`
}

// AdminAPI is the slice of the Keycloak admin REST API that onboarding needs.
// cmd/llz's kcClient satisfies it via a thin adapter; a fake satisfies it in
// tests. Kept deliberately minimal — the substrate seam for identity, mirroring
// provider.ClusterProvider for the cluster.
type AdminAPI interface {
	FindRealmRole(name string) (*Role, error)
	EnsureUser(rep UserRep) (id string, created bool, err error)
	AssignRealmRoles(userID string, roles []Role) error
	FindGroupID(name string) (id string, err error)
	AddUserToGroup(userID, groupID string) error
	SendUpdatePasswordEmail(userID string) error
}

// AddRequest is a resolved onboarding request: the user to create and the realm
// roles to grant (already expanded from --team/--admin by DesiredRoles).
type AddRequest struct {
	Username  string
	Email     string
	FirstName string
	LastName  string
	Roles     []string
	Invite    bool
}

// AddResult reports what AddUser did, for the caller to render. TempPassword is
// non-empty only when a fresh temporary password was set (temp-password path on a
// newly created user). GroupWarnings hold non-fatal group-mirroring problems the
// caller should surface.
type AddResult struct {
	Created       bool
	TempPassword  string
	Roles         []string
	GroupWarnings []string
}

// AddUser is the cluster-touching core (through api): validate the roles exist,
// create (or find) the user, grant the roles + any matching groups, and (invite
// path) send the set-password email. It never leaves an access-less orphan — a
// missing role fails BEFORE the user is created — and performs no I/O of its own
// beyond api calls.
func AddUser(api AdminAPI, req AddRequest) (AddResult, error) {
	res := AddResult{Roles: req.Roles}

	// Validate every role exists BEFORE creating the user, so a typo/unprovisioned
	// team fails fast without leaving an access-less orphan.
	reps := make([]Role, 0, len(req.Roles))
	for _, rn := range req.Roles {
		rep, err := api.FindRealmRole(rn)
		if err != nil {
			return res, fmt.Errorf("look up realm role %q: %w", rn, err)
		}
		if rep == nil {
			hint := "apl-core has not provisioned that team yet — declare it in spec.teams and render, or create it in the APL console"
			if rn == PlatformAdminRole {
				hint = "apl-core's built-in admin team role " + PlatformAdminRole + " is missing — is this a converged APL cluster?"
			}
			return res, fmt.Errorf("realm role %q does not exist: %s", rn, hint)
		}
		reps = append(reps, *rep)
	}

	// Create the user. Temp-password path sets an inline temporary credential +
	// forced UPDATE_PASSWORD; invite path creates the user password-less and emails
	// the set-password action afterwards.
	rep := UserRep{
		Username:        req.Username,
		Email:           req.Email,
		FirstName:       req.FirstName,
		LastName:        req.LastName,
		Enabled:         true,
		RequiredActions: []string{"UPDATE_PASSWORD"},
	}
	if req.Invite {
		rep.EmailVerified = false
	} else {
		// Temp-password login: mark the email verified so the user can sign in and
		// change the password without an SMTP round-trip.
		rep.EmailVerified = true
		res.TempPassword = RandomPassword()
		rep.Credentials = []Credential{{Type: "password", Value: res.TempPassword, Temporary: true}}
	}

	uid, created, err := api.EnsureUser(rep)
	if err != nil {
		return res, fmt.Errorf("create user %q: %w", req.Username, err)
	}
	res.Created = created
	if !created {
		res.TempPassword = "" // existing user — password left untouched (add-only)
	}

	// Grant the realm roles (idempotent), and add to any same-named group that
	// already exists so the console shows native membership.
	if err := api.AssignRealmRoles(uid, reps); err != nil {
		return res, fmt.Errorf("grant roles %v to %q: %w", req.Roles, req.Username, err)
	}
	for _, rn := range req.Roles {
		gid, err := api.FindGroupID(rn)
		if err != nil {
			res.GroupWarnings = append(res.GroupWarnings,
				fmt.Sprintf("could not look up group %q (role granted regardless): %v", rn, err))
			continue
		}
		if gid == "" {
			continue // group not created yet — the role alone carries the groups claim
		}
		if err := api.AddUserToGroup(uid, gid); err != nil {
			res.GroupWarnings = append(res.GroupWarnings,
				fmt.Sprintf("could not add %q to group %q (role granted regardless): %v", req.Username, rn, err))
		}
	}

	if req.Invite {
		if err := api.SendUpdatePasswordEmail(uid); err != nil {
			return res, fmt.Errorf("user %q created and roles granted, but the set-password email failed (is realm SMTP configured? use the default temp-password flow instead): %w", req.Username, err)
		}
	}
	return res, nil
}

// DesiredRoles resolves the deduped set of realm roles to grant from the --team
// names and --admin. At least one is required — a user with no team has no APL
// access.
func DesiredRoles(teams []string, admin bool) ([]string, error) {
	seen := map[string]bool{}
	var roles []string
	add := func(r string) {
		if !seen[r] {
			seen[r] = true
			roles = append(roles, r)
		}
	}
	for _, t := range teams {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		add("team-" + t)
	}
	if admin {
		add(PlatformAdminRole)
	}
	if len(roles) == 0 {
		return nil, fmt.Errorf("specify at least one --team <name> or --admin (a user with no team has no APL access)")
	}
	return roles, nil
}
