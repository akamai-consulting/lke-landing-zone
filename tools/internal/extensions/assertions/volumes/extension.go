package volumes

// extension.go — `assert-storage` declares itself.
//
// FOURTH EXTENSION, AND THE ONE THE CATALOG NOMINATED AS A PROBLEM. It is one of
// exactly two entries the catalog flags as breaking its own assertion rule —
// "holds `cloud-mutate`, the odd one out" — and the model's answer was that such
// an entry is not an exception but a MIS-DECLARATION: re-model it, and declare the
// mutating half honestly as its own binding. This is that claim, tested.
//
// It half survives. The re-modelling works and the escape hatch is real. But
// writing the honest declaration also produced the first thing the ceiling gets
// WRONG, which no amount of re-modelling fixes — see the note on the invariants.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `assert-storage` declaration.
//
// TWO BINDINGS. It was three until the volume-labels invariant was retired —
// renaming a bound Volume is what breaks the CSI's device lookup — and the pairing
// it demonstrated survives in the two that remain: a read-only assertion and a
// mutating invariant over the same resource, where `Extension.Grants()`'s derived
// union says something different from either binding alone.
//
//	assertion:verified  [cluster-read, cloud-read]   assert-volume-encryption
//	invariant:operating [cluster-read, cloud-mutate] volume-tags   (named)
//
// WHY THE MUTATING HALF IS AN INVARIANT AND NOT PART OF THE ASSERTION. This is
// the re-modelling working. `reconcile-volume-tags` is wired
// into `reconcile.go` as a continuous reconciler lane: it runs in-pod, forever,
// and PUTs tags onto Linode Volumes. Folding it into the assertion
// would have required an assertion that holds `cloud-mutate` — which the validator
// refuses, correctly, because an assertion that mutates what it measures cannot be
// trusted about what it found. Declared separately, each binding is judged on what
// it actually does and the assertion keeps its read-only ceiling.
//
// WHY THEY ARE NAMED. `operating` is the only state an invariant may attach to, so
// without Binding.Name an extension could hold exactly one. These are two — tags
// and labels are different lanes with different failure modes (a missing tag is a
// billing-attribution gap; a `pvc-<uuid>` label is an identity gap) and they fail
// independently. This is the first declaration in the repo that needs that field.
//
// THE CEILING IS WRONG ABOUT WHERE `cloud-mutate` MAY APPEAR, and this is the
// finding. `grantStates` lists CloudMutate at {provisioned, seeded, converged,
// destroyed} — NOT `operating` — so the two invariants above were refused when
// first written. The table's own header says it is "JUDGEMENT TRANSCRIBED, not a
// derived fact … the most likely thing here to need a row added, and adding one
// should be an argued change rather than a quiet widening." So, the argument:
//
//   - These two lanes SHIP. They are registered in `reconcile.go`, they run on
//     every cluster, and they mutate cloud resources continuously. The model was
//     refusing to describe code that is in production.
//   - The omission is traceable. The catalog placed `cloud-mutate` by reading the
//     `→ provisioned` and `→ destroyed` groups, where it is obviously needed, and
//     it flagged `assert-storage` as "the odd one out" WITHOUT following that
//     observation into the grant table. The table inherited the blind spot.
//   - Refusing it is not conservative, it is wrong in the dangerous direction. A
//     continuously-running cloud mutator is exactly the thing a reviewer most wants
//     declared; a ceiling that makes it inexpressible does not prevent it, it only
//     stops it being written down. That is `→ seeded` banned-by-omission again, in
//     the half of the ceiling that was added to fix banning-by-omission.
//
// `Operating` was therefore added to CloudMutate, with a regression test pinning
// the four original states so the row cannot be widened further by accident.
func Extension() extension.Extension {
	readCluster := []extension.Grant{extension.ClusterRead}
	return extension.Extension{
		Name:   "assert-storage",
		Short:  "every PV-backed Linode Volume is encrypted, tagged and named — and stays that way",
		Always: true,
		Bindings: []extension.Binding{
			{
				// The gate: reads the cluster through kubectl and the Volumes through
				// the Linode API, and mutates neither. `verified` because it attests a
				// property of a running platform rather than moving it anywhere.
				Kind:   extension.Assertion,
				State:  extension.Verified,
				Grants: append(readCluster, extension.CloudRead),
			},
			{
				// The tag healer. Volumes born untagged (a clone/snapshot PVC admitted
				// while admission control was degraded — the Linode clone API takes no
				// tags) are invisible to the billing and quota census until this runs.
				Kind:   extension.Invariant,
				Name:   "volume-tags",
				State:  extension.Operating,
				Grants: append(readCluster, extension.CloudMutate),
			},
		},
	}
}

// cloudBinding is the binding a Linode call is made under, chosen by NAME because
// this extension declares several with DIFFERENT cloud permissions.
//
// THE RULE IS THE NARROWEST BINDING THAT COVERS WHAT THE CALL ACTUALLY DOES, and
// it is applied by reading the HTTP verb rather than the function name. Both sites PUT (UpdateVolume, UpdateVolumeLabel), so both take a cloud-mutate
// invariant — and the two invariants are distinct, so each takes its own.
func cloudBinding(name string) extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Name == name {
			return b
		}
	}
	panic("assert-volumes: no binding named " + name + " — its Linode client is built from one, " +
		"so its absence is a wiring bug")
}
