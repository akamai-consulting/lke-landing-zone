package database

// extension.go — `database-provisioner` and `assert-database` declare themselves,
// the catalog's textbook capability/assertion pair.
//
// FORTY-THIRD AND FORTY-FOURTH. The model's own Binding doc names this pair as its
// strongest structural signal: "a capability and its assertion (harbor-provisioner
// ↔ assert-registry, database-provisioner ↔ assert-database) enable and disable
// together, which argues for one extension holding both bindings rather than two
// that must be kept in step by hand."
//
// AND THE CODE AGREES, which is worth saying because the same reasoning went the
// OTHER way for credential-pat and credential-objkey one extraction ago. Those two
// share a framework and nothing else: an instance with no object storage still
// wants PATs. These two share a SUBJECT — there is no sense in which an instance
// seeds a database admin credential but does not want to know whether it works,
// and an assertion about a database nobody provisioned has nothing to assert
// against. Same package, one extension, two bindings.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `database-provisioner` declaration.
//
//	transition:seeded    "seed-admin"   [cloud-read, secret-custody]
//	transition:seeded    "rotate-admin" [cloud-mutate, secret-custody]  ← pushed
//	assertion:verified   "admin-usable" [cloud-read, secret-read]
//
// THREE BINDINGS, AND THE MIDDLE ONE CARRIES A PRECONDITION. Rotation is a thing
// that keeps happening to a platform that is already up, not a step on the way to
// one — but `bindableStates` refuses a transition at `operating`, and rightly:
// `operating` is a condition that holds rather than a place you move to, and
// widening the rule would let something claim to move the platform THERE.
//
// So `seeded` is the State — a rotation re-places credential material, which is
// what seeding means — and `Requires: operating` carries what was missing, which
// was never the effect but the precondition. The mutating grants are checked at
// BOTH states, so this is not a way to ask at `operating` for something `seeded`
// forbids. See extension.Binding for the axis; `wedge-gameday` is the other case.
//
// `cloud-mutate` ON ROTATION, `cloud-read` ON SEEDING, and the asymmetry is real:
// Linode's admin-password reset is an API WRITE against a live database, whereas
// seeding reads the cluster list and writes only to OpenBao. Collapsing them would
// hide that seeding cannot break a running database and rotation can.
//
// THE ROTATION IS IRREVERSIBLE, which is why it refuses to run against an unseeded
// path. Linode resets the password in place — there is no mint-verify-swap — so a
// rotation whose OpenBao copy is missing an endpoint would store a password with
// nothing to use it against, and the old one is already gone. The guard is a
// carried-fields read that fails closed, and it is the reason this package needs
// baoread's fail-closed verdict rather than a plain "did the read return empty".
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "database-provisioner",
		Short:  "seed, rotate and prove the Postgres admin credential every deployment's database holds",
		Always: false,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Transition,
				Name:   "seed-admin",
				State:  extension.Seeded,
				Grants: []extension.Grant{extension.CloudRead, extension.SecretCustody},
			},
			{
				Kind:     extension.Transition,
				Name:     "rotate-admin",
				State:    extension.Seeded,
				Requires: extension.Operating,
				Grants:   []extension.Grant{extension.CloudMutate, extension.SecretCustody},
			},
			{
				Kind:   extension.Assertion,
				Name:   "admin-usable",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.CloudRead, extension.SecretRead},
			},
		},
	}
}

// namedBinding returns the binding with the given name, which is how each lane
// here gets the grants IT declared rather than the extension's union.
//
// THIS EXTENSION IS WHY THE LOOKUP IS BY NAME. It holds two transitions at
// `seeded` — seed-admin and rotate-admin — plus an assertion. Building a handle
// from "the transition" would pick whichever came first and silently give one
// lane the other's grants; building from the extension would hand both the union,
// which is the over-granting the per-binding model exists to prevent.
func namedBinding(name string) extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Name == name {
			return b
		}
	}
	panic("database-provisioner: no binding named " + name + " — its OpenBao handle is built " +
		"from that binding, so the name going stale is a wiring bug, not a missing feature")
}

// cloudBinding is the binding a Linode call is made under, chosen by NAME because
// this extension declares several with DIFFERENT cloud permissions.
//
// THE RULE IS THE NARROWEST BINDING THAT COVERS WHAT THE CALL ACTUALLY DOES, and
// it is applied by reading the HTTP verb rather than the function name. `rotate-admin` holds cloud-mutate and needs it: dbAdminAPI carries
// ResetPostgresCredentials alongside the two GETs.
func cloudBinding(name string) extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Name == name {
			return b
		}
	}
	panic("database-admin: no binding named " + name + " — its Linode client is built from one, " +
		"so its absence is a wiring bug")
}
