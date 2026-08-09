package assertregistry

// extension.go — `assert-registry` declares itself.
//
// SEVENTEENTH EXTENSION, THE CHEAPEST OF ALL OF THEM, AND THE ONLY ONE THAT NEEDS
// NO INJECTED CAPABILITIES. It measured a closure of 2 — both entries noise —
// because everything it does is an OCI distribution v2 handshake over net/http
// against a public registry endpoint, and the one cluster read it needs already
// had a home and a seam in internal/harborauth.
//
// A `Deps` struct was written for this package and deleted. See the note on Run:
// it duplicated seams that already existed, which is the third time in three
// extractions that a capability field turned out to be a parser or an
// already-swappable var wearing a seam's clothes.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `assert-registry` declaration.
//
//	assertion:verified "harbor-roundtrip" [cluster-read, secret-read]
//
// ONE BINDING, because there is one question: can a minted Harbor robot actually
// authenticate for pull AND push? Splitting it would be splitting a handshake.
//
// WHY secret-read AND NOT secret-custody. It reads the robot credential Secret and
// uses the credential to log in. That is reading credential material, not placing
// it — the distinction `token-inventory` forced into the vocabulary two
// extractions ago, and this is the first extension declared AFTER the split that
// needs the read half. Under the old single word this binding would have been
// inexpressible for the same reason `validate-tokens` was: an assertion permits
// read grants only, and `secret-custody` was half a write grant.
//
// THE SCAR THIS LANE EXISTS FOR, because it explains the grant. Managed instances
// once rendered HARBOR_HOST as "harbor." — non-empty, so it defeated every
// empty-string guard including the systeminfo fallback — and every push and pull
// 401'd. Every credential in the chain was valid; the HOST was wrong. Nothing
// caught it because nothing ever USED the credential: the provisioner asserted it
// had CREATED a robot, not that the robot could log in. An assertion that only
// reads metadata could not have caught this. It has to hold the credential and
// try.
//
// PAIRS WITH `harbor-provisioner`, which is the second of the catalog's four
// capability/assertion pairs to have half of it land. `assert-reconciler` settled
// the question of whether such pairs should merge — they should not, because the
// merged grant line is the union — and this one is a cleaner illustration than
// that one was: the provisioner will hold cloud-mutate and secret-custody to MINT
// the robot; this holds cluster-read and secret-read to USE it. Nothing in the
// union would be true of either half.
//
// No ceiling change. Assertions may bind any state, and both grants are read-only.
func Extension() extension.Extension {
	return extension.Extension{
		Name:  "assert-registry",
		Short: "fail unless a minted Harbor robot can actually authenticate for pull and push",
		// OPT-IN: an instance without Harbor has no robot to exercise.
		Always: false,
		Bindings: []extension.Binding{{
			Kind:   extension.Assertion,
			Name:   "harbor-roundtrip",
			State:  extension.Verified,
			Grants: []extension.Grant{extension.ClusterRead, extension.SecretRead},
		}},
	}
}
