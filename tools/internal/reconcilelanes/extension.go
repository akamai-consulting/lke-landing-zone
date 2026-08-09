package reconcilelanes

// extension.go — `reconcile-actions` declares itself.
//
// FIFTH EXTENSION, AND THE ONE THE CATALOG CALLS "the clearest case for
// one-invariant-per-extension" — seven separate invariants whose needs differ,
// where collapsing them widens the grants to their union. That is the claim under
// test here, and it is the first one the model gets unambiguously RIGHT: four
// lanes, four named bindings, and two distinct grant sets that a single binding
// could not have expressed without over-granting.
//
// It is also the first extension where what could NOT be extracted is as
// informative as what could. Four of the eight lanes stayed in package main, and
// their reasons are recorded below rather than smoothed over — the extension is
// honestly partial, and the model has no way to say so, which is itself a finding.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `reconcile-actions` declaration.
//
//	invariant:operating [cluster-read, cluster-write]  sc-demote
//	invariant:operating [cluster-read, cluster-write]  argo-nudge
//	invariant:operating [cluster-read, cluster-write]  es-store-recovery
//	invariant:operating [cluster-read, secret-custody] openbao-gauges
//
// WHY THE SPLIT IS LOAD-BEARING, in one number: collapse these four into a single
// binding and it must hold the UNION — `cluster-write` *and* `secret-custody`. The
// OpenBao sampler would then carry permission to patch StorageClasses and Argo
// Applications, and the three cluster lanes would carry an OpenBao token. Neither
// needs the other's capability, and the sampler is the one that most obviously
// must not have it: it is read-only by construction and its whole value is that a
// compromised gauge cannot change anything. The catalog said splitting them
// mattered; this is the concrete cost of not.
//
// `openbao-gauges` IS THE FIRST `secret-custody` BINDING in the registry. It
// authenticates to OpenBao through the reconciler's k8s-auth role and reads seal
// state plus credential rotation metadata — real custody of a real token, fenced
// by `policyReconcilerRead` to metadata reads on the paths in CredPaths. The
// grantStates table already permitted `secret-custody` at `operating`, so unlike
// `cloud-mutate` in `assert-storage` this one needed no ceiling change; it is the
// control case showing the table is not simply permissive everywhere.
//
// FOUR OF THE EIGHT LANES DID NOT MOVE, and the reasons are three different kinds:
//
//   - `tokens` — 15 references into ci_token_inventory.go, which is the
//     `token-inventory` extension's territory (a separate catalog entry, 1,473
//     lines). Extracting the lane means extracting the inventory it restores from.
//     This is the closure census's thesis at its sharpest: the lane is 236 lines
//     and its dependency is six times that.
//   - `apl-overlay` and `apl-overlay-wait` — 9 references, and they share this
//     package's OpenBao client seam (OpenBaoClientFn / OpenBaoLoginFn /
//     OpenBaoJWTFn, exported for exactly that reason). They are one credential
//     cluster with `openbao-gauges` and would move together; splitting the cluster
//     to move half of it would have meant duplicating the seam.
//   - `linode-token-wait` — did not move because it is NOT A LANE. It is a watch
//     WRAPPER: it kicks another lane's watch when the Linode token first appears,
//     and never acts itself. The catalog counts it as one of the seven invariants
//     and it is a scheduling concern belonging to the runtime. A miscount, found
//     only by trying to declare it.
//
// WHAT THE MODEL CANNOT SAY: that this extension is PARTIAL. `reconcile-actions`
// is declared with four bindings and reads as complete; nothing distinguishes "has
// four invariants" from "has eight, four of which are still in core". Every
// extraction from here on will pass through this state, and an extension that
// silently under-declares its own surface is the same failure shape as banning by
// omission — the reader cannot tell what is missing. Recorded, not fixed: the
// remedy is probably a declaration-level "incomplete" marker, and inventing one
// from a single case is what the `write-repo` deferral was right to avoid.
func Extension() extension.Extension {
	cluster := []extension.Grant{extension.ClusterRead, extension.ClusterWrite}
	return extension.Extension{
		Name:   "reconcile-actions",
		Short:  "the llzReconciler's action lanes — each an invariant that must keep holding",
		Always: true,
		Bindings: []extension.Binding{
			{
				// LKE's Flux-managed HelmRelease keeps re-marking the retain
				// StorageClass as cluster default; two defaults hard-fail converge,
				// and the Kyverno policy is admission-only so it starves once Flux
				// goes quiet. This is the durable backstop.
				Kind: extension.Invariant, Name: "sc-demote",
				State: extension.Operating, Grants: cluster,
			},
			{
				// Re-triggers Argo comparisons stuck on a TRANSIENT fetch error.
				// The transient/permanent rule is internal/health's, shared with
				// `llz ci assert-argo-app` so the two cannot disagree.
				Kind: extension.Invariant, Name: "argo-nudge",
				State: extension.Operating, Grants: cluster,
			},
			{
				// ExternalSecrets whose store was unready at first sync never retry
				// on their own; this force-syncs them once the store recovers.
				Kind: extension.Invariant, Name: "es-store-recovery",
				State: extension.Operating, Grants: cluster,
			},
			{
				// Read-only by construction: seal state and credential rotation age.
				// cluster-read is for the ServiceAccount JWT it exchanges; the
				// custody is of the OpenBao token that exchange returns.
				Kind: extension.Invariant, Name: "openbao-gauges",
				State: extension.Operating,
				Grants: []extension.Grant{
					extension.ClusterRead, extension.SecretCustody,
				},
			},
		},
		Incomplete: []string{
			"invariant:operating/tokens — 15 references into ci_token_inventory.go, which is " +
				"the token-inventory extension's territory (a separate catalog entry of 1,473 lines)",
			"invariant:operating/apl-overlay and /apl-overlay-wait — one credential cluster with " +
				"openbao-gauges, sharing this package's OpenBao client seam",
			"linode-token-wait is NOT missing: it is a watch wrapper belonging to the runtime, " +
				"and the catalog counting it among the seven invariants is a miscount",
		},
	}
}
