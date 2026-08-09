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
// THREE BINDINGS, AND THE MIDDLE ONE IS PUSHED — the SECOND case of a shape
// `wedge-gameday` found and could not name.
//
// Rotation belongs at `operating`. It is a thing that keeps happening to a
// platform that is already up, on a schedule, not a step on the way to one. And
// `bindableStates` refuses:
//
//	a transition binding cannot attach to "operating"
//
// ITS REASONING IS ABOUT REACHING A STATE: "`operating` is a condition that holds
// rather than a place you move to." That is right, and it is not what this
// binding claims. The rule cannot currently tell "moves the platform TO state X"
// from "acts WHILE the platform is at state X" — and the second is what a
// scheduled rotation, and a gameday, both are.
//
// TWO INDEPENDENT SHIPPING CASES NOW, and the shape is finally precise: a MUTATING
// ACTION WHOSE PRECONDITION IS A STATE RATHER THAN WHOSE EFFECT IS THAT STATE.
// `wedge-gameday` refuses to start unless the cluster is already Healthy; this
// refuses to rotate an unseeded path. Neither moves the platform anywhere.
//
// STILL NOT WIDENED, and deliberately. Letting Transition reach `operating` would
// buy this binding accuracy at the cost of the rule's actual content — something
// could then claim to move the platform TO `operating`, which is the thing the
// restriction exists to prevent. What these two want is a way to declare a
// precondition, which is a different axis from kind or state, and inventing an
// axis from two cases is further than this campaign has gone for anything. It is
// the same disposition `write-repo` got for three extractions before the fourth
// case made its shape unarguable.
//
// AND THE TWO TABLES DISAGREE ABOUT WHERE THIS GOES, which is the sharpest thing
// this extraction found. `converged` was the obvious fallback and grantStates
// refuses it: `secret-custody` is legal only at provisioned, seeded and operating.
// Meanwhile that row's own comment says it was written for "credentials the
// platform MINTS (seeding) or REPLACES (rotation)" — rotation is the reason
// `operating` is in it.
//
// So grantStates EXPECTS a rotation at `operating`, and bindableStates forbids a
// transition there. They are not strictly contradictory — an INVARIANT at
// `operating` can hold custody, and reconcile-actions' token restorers are exactly
// that — but a periodic rotation is not an invariant either: it does not hold
// continuously, it happens on a schedule.
//
// So `seeded`, the one state both tables allow, and defensible on its own terms: a
// rotation re-seeds the credential. Recorded in Incomplete as the approximation it
// is, with a test pinning both refusals so this is revisited deliberately.
//
// `cloud-mutate` ON ROTATION, `cloud-read` ON SEEDING, and the asymmetry is real:
// Linode's admin-password reset is an API WRITE against a live database, whereas
// seeding reads the cluster list and writes only to OpenBao. That distinction is
// what the two grants are for, and collapsing them would hide that seeding cannot
// break a running database and rotation can.
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
				Kind:   extension.Transition,
				Name:   "rotate-admin",
				State:  extension.Seeded,
				Grants: []extension.Grant{extension.CloudMutate, extension.SecretCustody},
			},
			{
				Kind:   extension.Assertion,
				Name:   "admin-usable",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.CloudRead, extension.SecretRead},
			},
		},
		Incomplete: []string{
			"rotate-admin is bound at `seeded`, which is the one state BOTH tables allow. Its " +
				"true home is `operating`: a scheduled rotation runs against a platform that " +
				"is already up. bindableStates refuses a transition there (rightly, for a rule " +
				"about REACHING a state — but this binding does not move the platform anywhere, " +
				"it requires the platform to already be there), and grantStates then refuses " +
				"secret-custody at `converged`, the obvious fallback. Note that grantStates' own " +
				"comment says `operating` is in the custody row FOR rotation — the two tables " +
				"disagree about this binding. Second case of the shape after wedge-gameday; what " +
				"both want is a way to declare a PRECONDITION, which is a different axis from " +
				"kind or state, and nothing was invented from two cases.",
		},
	}
}
