package argodiag

// extension.go — `argocd-diagnostics` declares itself, and declares what it
// CANNOT say.
//
// TWENTY-NINTH EXTENSION, AND THE FIRST WHOSE BINDING KIND IS WRONG ON PURPOSE.
// Every extraction so far has found a declaration that is true once the right
// words are chosen. This one has no true kind, and the four available are wrong in
// four different ways:
//
//   - `gate` runs before a state is attempted, over FILES ALONE — Validate()
//     rejects a gate holding anything but read-repo, and this reads a cluster.
//   - `transition` ACTS to move the platform into a state. This changes nothing;
//     it was grepped for writes before declaring, and unlike the last five
//     `assert-` lanes it really is read-only.
//   - `invariant` must hold CONTINUOUSLY at `operating`. This runs once, on
//     failure, and holds nothing.
//   - `assertion` contributes evidence that a state HOLDS — and that is the one
//     declared below, under protest.
//
// WHY IT IS WRONG. This command's own help text says "Always exits 0: diagnostics
// must never mask the failure that triggered them", and that is not a detail of
// its implementation, it is its PURPOSE. An assertion that cannot fail contributes
// a constant `true`. Worse, it runs precisely when the state does NOT hold: it is
// the thing you run after `converged` failed, to find out why. Declared as an
// assertion it is evidence for exactly the conclusion its existence contradicts.
//
// WHY IT IS DECLARED ANYWAY, AND MARKED. `Incomplete` exists for this — an
// extension that silently under-declares its own surface is the same failure shape
// as PR #15's ban-by-omission, where the reader cannot tell what is missing. The
// alternative was to invent a fifth binding kind on the first case that wanted
// one, and this campaign's own rule says otherwise: a gap gets a new word when a
// declaration is IMPOSSIBLE and there are two independent shipping cases. This is
// case one.
//
// CASES TWO AND THREE ARE ALREADY VISIBLE IN THE CATALOG, which is why this is
// recorded rather than shrugged off: `doctor-probes` (230 lines, 3 files) and
// `phase-timing` (316 lines, 2 files) are the same shape — read something, print
// it for a human, never fail. When one of them is extracted the fifth kind can be
// argued from three instances instead of guessed from one, and the shape question
// it has to answer is already clear: does a diagnostic attach to a STATE at all,
// or to the FAILURE of one? `write-repo` took four cases and three refusals to get
// right, and the refusals are what made the eventual word well-shaped.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `argocd-diagnostics` declaration.
//
//	assertion:converged[cluster-read]   — see the file header; the kind is wrong
//
// `cluster-read` and nothing else is the one part of this that is exactly true.
// The extraction confirmed it rather than assuming it: every probe is a `kubectl
// get`, `describe`, `logs` or `helm status`, there is no apply/patch/delete/create
// anywhere in the package, and TestPackageStaysReadOnly fails if that changes.
//
// That matters more here than usual. FIVE consecutive `assert-`-prefixed lanes
// turned out to hide a cluster write (converge, assert-storage,
// assert-observability, assert-secrets, assert-identity), which is why grepping
// for writes before declaring became settled practice. This is the first candidate
// in that run whose name promised observation and delivered it — and its name is
// `diagnose`, not `assert`.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "argocd-diagnostics",
		Short:  "dump apl-operator + ArgoCD failure diagnostics (best-effort, never fails)",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Assertion,
			State:  extension.Converged,
			Grants: []extension.Grant{extension.ClusterRead},
		}},
		Incomplete: []string{
			"the binding kind is wrong: this is a DIAGNOSTIC, not an assertion. It always " +
				"exits 0 by design and runs precisely when `converged` did NOT hold, so it " +
				"contributes no evidence that any state holds. The model has no fifth kind " +
				"for 'read a failing platform and print it for a human'. This is case one; " +
				"`doctor-probes` and `phase-timing` are the same shape and are the cases the " +
				"kind should be argued from.",
		},
	}
}
