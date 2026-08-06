package assertidentity

// extension.go — `assert-identity` declares itself.
//
// TWENTY-SEVENTH EXTENSION, AND THE FIRST WHOSE BOUNDARY GO CHOSE RATHER THAN THE
// DESIGN. The team-login smoke lane defines METHODS on the Keycloak admin client
// — realmRoleExists, findGroupID, ensureDirectGrantClient — and Go will not let a
// package add methods to a type it does not own. So this extension could not be
// extracted at all while the client lived in package main. The choice was to move
// the client out, or to merge this with `keycloak-provisioner`, which the branch
// has already decided should stay separate.
//
// internal/keycloak came out as the thirteenth shared package. The previous twelve
// came out on caller count (guardwalk at ten, cigate at twelve) or to break a
// dependency cycle (instancelayout). This one came out because the LANGUAGE
// required it — and then kept pulling: the provisioner's methods had to follow,
// then `llz users add`'s, then the smoke lane's own, because every operation on a
// shared type must live with it once a second caller appears.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `assert-identity` declaration.
//
//	assertion:verified   "certificates" [cluster-read]
//	transition:converged "login-smoke"  [cluster-read, cluster-write, secret-read]
//
// FIVE FOR FIVE ON HIDDEN MUTATIONS. Grepping for writes before declaring is
// settled practice now, and it paid again: the smoke lane CREATES a Keycloak
// direct-grant client and a temporary user, drives a login through them, and
// deletes both. You cannot prove a team can log in without a login to make — but
// it is still a mutation, reached from a lane called `smoke`.
//
// It writes to Keycloak rather than the Kubernetes API, and `cluster-write` is the
// closest the vocabulary comes. That is a slight stretch: the same gap
// `assert-observability` recorded when its Loki flush POST fit no grant. TWO
// INSTANCES NOW — an external service that the platform owns, mutated by a lane
// that otherwise observes. A third would make it worth a word.
//
// WHY certificates IS SEPARATE. It lists cert-manager Certificates and checks
// their Ready condition. No writes, no credential, no Keycloak. Folding it into
// the smoke binding would hand a read-only check the power to create users.
//
// WHY secret-read. The smoke lane reads the platform-admin credential out of a
// Secret to authenticate as an admin before it creates anything.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "assert-identity",
		Short:  "certificates are Ready, and a team member can actually log in",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Assertion,
				Name:   "certificates",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead},
			},
			{
				// Creates a direct-grant client and a temporary user, then removes
				// both. Inherent to proving a login works, and still a write.
				Kind:  extension.Transition,
				Name:  "login-smoke",
				State: extension.Converged,
				Grants: []extension.Grant{
					extension.ClusterRead,
					extension.ClusterWrite,
					extension.SecretRead,
				},
			},
		},
	}
}
