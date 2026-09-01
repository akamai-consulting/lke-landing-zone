package converge

// extension.go — `converge` declares itself.
//
// FOURTEENTH EXTENSION, AND THE ACID TEST. The catalog named it that two hundred
// declarations ago, on the grounds that `ci_health.go` fuses the converge ACTION
// with the health PREDICATE and the model would have to separate them. It does,
// and the separation was available rather than invented: `internal/health` was
// already 1,164 lines of pure classification, and the commands were already the
// kubectl orchestration feeding it. The catalog's prediction — "that library half
// is an assertion:converged; the command half is the transition:converged" — is
// what this file writes down.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `converge` declaration.
//
//	transition:converged "drive"        [cluster-read, cluster-write]
//	assertion:converged  "health"       [cluster-read]
//	assertion:converged  "health-incluster" [cluster-read]
//
// THE SPLIT IS THE POINT, and it is not cosmetic. `llz ci health` reports a
// verdict and a second run changes nothing. `llz ci converge` polls toward that
// verdict AND acts on what it sees: it nudges Argo Applications (a patch), it
// strips oversized last-applied-configuration annotations off CRDs when a sync
// hits the 256KB limit, it restarts argocd-redis on a repo-server auth split, and
// it DELETES an object whose declared shape the API server will not accept in
// place (`--cascade=orphan`, so the pods keep running, and only for migrations
// declared safe to automate). Those are cluster writes performed from inside what
// looks like a health check, and before this declaration nothing said so anywhere.
//
// THE ORPHAN DELETE IS THE HEAVIEST THING THIS BINDING DOES, so it is named here
// rather than left to be discovered in health.go. It is on the same footing as the
// other three: a fault the loop cannot poll its way out of, because the desired
// state is correct and the cluster is correct to refuse it. What separates it is
// blast radius, which is why it is bounded: once per run PER MIGRATION (the ids
// attempted are recorded, so neither an unreadable sibling nor a refused delete
// can turn "once" into "every poll"), platform scope only,
// per-migration opt-in (brownfield.Migration.Auto), never on a poll that has just
// established Argo cannot sync, never unless the Application that OWNS the object
// is Synced and can compute its target state (the recreate is Argo's to perform,
// and an object deleted where nothing will put it back is a one-way door), and
// the verdict is re-taken afterwards rather than consumed — a run must not end on
// a reading that predates its own delete. A repair the apiserver refuses fails the
// run rather than being annotated past. `--brownfield-migrate=false` turns it off.
//
// A reviewer reading `llz ci health --wait` would reasonably assume it observes.
// The grant line is the correction: `drive` holds cluster-write, `health` does
// not, and the two cannot be confused because they are separate bindings.
//
// WHY `health-incluster` IS ITS OWN BINDING RATHER THAN A THIRD GRANT ON `health`.
// Same verdict, same grants, different REACH: it runs on the distroless image with
// no kubectl, over internal/kube, authenticated by the pod ServiceAccount. Naming
// it separately is what makes "there are two ways to compute this verdict and they
// must agree" visible in the declaration rather than only in a comment. The
// catalog listed it under `health-sla` purely because of its filename; the twelfth
// extension sent it back here, where its convergence contract already lived.
//
// NO CEILING CHANGE. `cluster-write` was already legal at `converged` — that state
// exists in grantStates for exactly this transition — and `cluster-read` is
// unrestricted. The acid test did not break the model. What it strained was the
// ACTION ABI's absence: see deps.go on why this extension's capabilities are
// installed rather than threaded, and why that is the strongest argument yet for
// dispatch handing each binding its own handle.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "converge",
		Short:  "drive the platform to convergence, and report whether it got there",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Transition,
				Name:   "drive",
				State:  extension.Converged,
				Grants: []extension.Grant{extension.ClusterRead, extension.ClusterWrite},
			},
			{
				Kind:   extension.Assertion,
				Name:   "health",
				State:  extension.Converged,
				Grants: []extension.Grant{extension.ClusterRead},
			},
			{
				Kind:   extension.Assertion,
				Name:   "health-incluster",
				State:  extension.Converged,
				Grants: []extension.Grant{extension.ClusterRead},
			},
		},
	}
}
