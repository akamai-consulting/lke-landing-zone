package assertreconciler

// extension.go — `assert-reconciler` declares itself.
//
// SIXTEENTH EXTENSION, AND THE SECOND OPT-IN ONE. `import-brownfield` was the
// first; everything between them shipped enabled on every instance. This one is
// off by default for a reason the catalog names: it asserts about the in-cluster
// reconciler, and an instance that does not run one has nothing for it to judge.
//
// THAT MAKES IT THE FIRST HALF OF THE CATALOG'S "PAIRING PATTERN" TO LAND.
// `reconciler-runtime` ↔ `assert-reconciler` is one of four capability/assertion
// pairs the catalog identified, with the observation that "a capability and its
// assertion turn on and off together" — and the suggestion that one extension
// carrying both bindings might beat two extensions. This extraction is evidence
// for the two-extension reading: see the Deps note.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `assert-reconciler` declaration.
//
//	assertion:operating "functional-health" [cluster-read]
//	assertion:operating "effects"           [cluster-read]
//
// WHY `operating` AND NOT `verified`. Both lanes ask about a reconciler that is
// ALREADY RUNNING and expected to keep running: is the loop up, does a replica
// hold the driving Lease, are the enabled lanes fresh, and are their effects
// actually landing in the cluster. That is drift-detection against a live system,
// which is what `operating` means here. `verified` would say these run once after
// a transition and then stop mattering, and the reconciler is precisely the thing
// that never stops mattering.
//
// Note they are ASSERTIONS at `operating`, not invariants. An invariant is a
// property this extension MAINTAINS; these only observe. `reconcile-actions`
// holds the invariants — the lanes themselves — and this holds the judgement of
// whether they worked. Same state, opposite side of the fence.
//
// WHY TWO BINDINGS. `functional-health` reads metrics and the Lease: is the
// reconciler alive and leading? `effects` reads the CLUSTER: did the lanes' work
// actually land? A reconciler can be perfectly healthy by the first measure and
// producing nothing by the second — that is the whole reason the effects lane was
// written — so collapsing them would hide the failure the pair exists to separate.
//
// EVIDENCE FOR TWO EXTENSIONS RATHER THAN ONE. The catalog wondered whether a
// capability and its assertion should be one extension carrying both bindings.
// After extracting this half: they should not. `reconciler-runtime` will hold
// cluster-write, secret-custody and a leader election; this holds cluster-read and
// nothing else. Merging them would produce a single declaration whose grant line
// is the union — which is exactly the over-granting per-binding grants exist to
// prevent, and it would mean the read-only assertion could no longer be reasoned
// about separately from the process it judges. The pairing is real; it is an
// ENABLEMENT relationship, not an identity.
//
// No ceiling change. Assertions may bind any state, and `cluster-read` is
// unrestricted.
func Extension() extension.Extension {
	return extension.Extension{
		Name:  "assert-reconciler",
		Short: "assert the in-cluster reconciler is alive, leading, and that its lanes' effects land",
		// OPT-IN: an instance that does not run a reconciler has nothing here to
		// assert about, and a lane that always fails on such an instance is a lane
		// operators learn to ignore.
		Always: false,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Assertion,
				Name:   "functional-health",
				State:  extension.Operating,
				Grants: []extension.Grant{extension.ClusterRead},
			},
			{
				Kind:   extension.Assertion,
				Name:   "effects",
				State:  extension.Operating,
				Grants: []extension.Grant{extension.ClusterRead},
			},
		},
	}
}
