package reconcilelanes

// extension.go — `reconcile-actions` declares itself.
//
// FIFTH EXTENSION, AND THE ONE THE CATALOG CALLS "the clearest case for
// one-invariant-per-extension" — seven separate invariants whose needs differ,
// where collapsing them widens the grants to their union. That is the claim under
// test here, and it is the first one the model gets unambiguously RIGHT: FIVE
// lanes, five named bindings, and three distinct grant sets that a single binding
// could not have expressed without over-granting.
//
// It is also the first extension where what could NOT be extracted is as
// informative as what could. Three of the eight lanes are still unextracted (they
// live in internal/cli, not in package main, which is now 29 lines), and their
// reasons are in Incomplete rather than smoothed over.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `reconcile-actions` declaration.
//
//	invariant:operating [cluster-read, cloud-mutate, secret-custody] linode-creds
//	invariant:operating [cluster-read, cluster-write]                sc-demote
//	invariant:operating [cluster-read, cluster-write]                argo-nudge
//	invariant:operating [cluster-read, cluster-write]                es-store-recovery
//	invariant:operating [cluster-read, secret-custody]               openbao-gauges
//
// WHY THE SPLIT IS LOAD-BEARING, in one number: collapse these five into a single
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
// by `policyReconcilerRead` to metadata reads on the paths in credpaths.CredPaths. The
// grantStates table already permitted `secret-custody` at `operating`, so unlike
// `cloud-mutate` in `assert-storage` this one needed no ceiling change; it is the
// control case showing the table is not simply permissive everywhere.
//
// THREE OF THE EIGHT LANES HAVE NOT MOVED, and the reasons are three different
// kinds. (`linode-creds` was a fourth until the coupling test found it undeclared;
// see its binding below.)
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
// WHAT THE MODEL COULD NOT SAY, AND NOW CAN: that this extension is PARTIAL. It
// read as complete because nothing distinguished "has five invariants" from "has
// eight, three of which are unextracted" — the same failure shape as banning by
// omission, since the reader cannot tell what is missing. This extension was the
// first of the two independent cases that bought `Extension.Incomplete`
// (`template-sustain` was the second); the notes below are that field in use, and
// `llz extension list` marks the extension `◐` off them.
func Extension() extension.Extension {
	cluster := []extension.Grant{extension.ClusterRead, extension.ClusterWrite}
	return extension.Extension{
		Name:   "reconcile-actions",
		Short:  "the llzReconciler's action lanes — each an invariant that must keep holding",
		Always: true,
		// FOLLOWS `llzReconciler`. These are the daemon's action lanes — they run inside
		// the process the component deploys, so their enablement cannot differ from its.
		Component: "llzReconciler",
		Bindings: []extension.Binding{
			{
				// FOUND BY THE COUPLING TEST, NOT BY REVIEW. This lane rotates the
				// in-cluster Linode object-storage credentials on a timer
				// (credrotate.RunRotateLinodeCreds) and had NO declaration at all —
				// a continuously-running credential mutator outside the model, so
				// invisible to `llz extension list`, to enablement and to the
				// capability fence. reconciler_registry_test.go is what said so.
				//
				// It reads as a rotation, and rotation is declared elsewhere as
				// `transition:seeded` — which says the mutation happens ONCE, at
				// seeding. This one runs forever, which is what makes it an
				// invariant rather than a transition that someone forgot to move.
				//
				// cloud-mutate mints and revokes the keys; secret-custody writes them
				// into OpenBao. Both are legal at `operating` (grantStates), and both
				// are what the lane's own code does.
				Kind: extension.Invariant, Name: "linode-creds",
				State: extension.Operating,
				Grants: []extension.Grant{
					extension.ClusterRead, extension.CloudMutate, extension.SecretCustody,
				},
			},
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
			"invariant:operating/tokens — 15 references into what is now the token-inventory " +
				"extension (assertions/tokeninv), a separate catalog entry of 1,473 lines. " +
				"Extracting the lane means extracting the inventory it restores from",
			"invariant:operating/apl-overlay and /apl-overlay-wait — one credential cluster with " +
				"openbao-gauges, sharing this package's OpenBao client seam",
			"linode-token-wait is NOT missing: it is a watch wrapper belonging to the runtime, " +
				"and the catalog counting it among the seven invariants is a miscount",
		},
	}
}
