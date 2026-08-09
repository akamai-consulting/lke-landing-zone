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

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `assert-storage` declaration.
//
// THREE BINDINGS, and it is the first extension with more than one — so it is also
// the first to exercise the pairing pattern the catalog calls its strongest
// structural signal, and the first where `Extension.Grants()`'s derived union says
// something different from any single binding.
//
//	assertion:verified  [cluster-read, cloud-read]   assert-volume-encryption
//	invariant:operating [cluster-read, cloud-mutate] volume-tags   (named)
//	invariant:operating [cluster-read, cloud-mutate] volume-labels (named)
//
// WHY THE MUTATING HALVES ARE INVARIANTS AND NOT PART OF THE ASSERTION. This is
// the re-modelling working. `reconcile-volume-tags` and `relabel-volumes` are wired
// into `reconcile.go` as continuous reconciler lanes: they run in-pod, forever,
// and they PUT tags and labels onto Linode Volumes. Folding them into the assertion
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
			{
				// The relabeler. A `pvc-<uuid>` label is the Volume's identity in the
				// Linode UI, the billing export and the quota census; once the cluster
				// is gone nothing can attribute it.
				Kind:   extension.Invariant,
				Name:   "volume-labels",
				State:  extension.Operating,
				Grants: append(readCluster, extension.CloudMutate),
			},
		},
	}
}
