package teardown

// extension.go — `teardown` declares itself.
//
// SIXTH EXTENSION, AND THE FIRST `transition`. Five extensions in, no binding had
// ever MOVED the platform anywhere — every one observed (`gate`, `assertion`) or
// held (`invariant`). This was picked to close that, and picked over the two other
// transition candidates precisely because it lands in NO known-open gap: it needs
// no `write-repo`, and it is not partial. If a transition binding misbehaves here,
// the transition is the reason.
//
// It also declares the model's own motivating example. `bindableStates` lets an
// assertion target any state, and the justification written into validate.go is
// `assert-no-orphans` — "the assertion that `destroyed` actually holds, and the one
// a missed Volume bills for". That argument had never been tested against code.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `teardown` declaration.
//
//	transition:destroyed [cloud-mutate, cluster-write]  the destroy itself
//	assertion:destroyed  [cloud-read]                   assert-no-orphans
//
// THE TRANSITION WORKS, AND `destroyed` IS WHERE IT MATTERS. A transition is the
// only kind that may target `destroyed` besides an assertion, and `cloud-mutate`
// was already legal there — the one state in grantStates that exists purely for
// this. So the first transition needed no ceiling change at all, which is the
// third consecutive data point that the table is right more often than not.
//
// `cluster-write` is for the UNWEDGE half: a namespace stuck terminating on a
// finalizer, or a CRD whose last-applied annotation exceeds the object cap, will
// hang a destroy forever. Stripping those is a cluster mutation performed in the
// service of a destroy, and it is a fair test of whether one binding may hold two
// mutating grants — it may, and should, because both are the same action.
//
// THE ASSERTION IS READ-ONLY, and that is not cosmetic here. `assert-no-orphans`
// is the destroy job's FINAL gate: it re-counts what the destroy was supposed to
// remove. An assertion that could also delete would be able to make its own
// verdict true, which is the exact thing the read-only ceiling protects — and this
// is the case where it would be most tempting, since "just reap the leftovers" is
// one call away. Declared separately, the reaper is the transition's job and the
// gate only counts. A test pins it.
//
// WHAT THIS EXTRACTION FOUND THAT THE MODEL HAS NO WORD FOR: `Deps.Confirm`.
//
// Every seam the previous five needed delivers a CAPABILITY — a token, a client, a
// writer. Confirm delivers an AUTHORISATION: `--yes`, the answer to "may I", not
// the means to act. The grant vocabulary cannot express it. `cloud-mutate` says
// this binding MAY delete cloud resources; it says nothing about whether a human
// agreed to THIS deletion, and the two are different questions — a destroy verb
// that is granted but unconfirmed must dry-run, not proceed.
//
// It has not come up before because none of the first five destroys anything. It
// is the third thing the model cannot say (after `write-repo` and PARTIAL), and
// unlike those two it is not obviously a missing grant: "granted" and "confirmed"
// may want to be different axes entirely. Recorded, not invented — the action ABI
// is where it has to be answered, because a binding that holds `cloud-mutate` at
// `destroyed` is exactly where the two bits must not be one.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "teardown",
		Short:  "destroy an environment's substrate, and prove nothing leaked",
		Always: true,
		Bindings: []extension.Binding{
			{
				// Delete the cluster, its firewall and its VPC; detach and sweep
				// the Volumes; unwedge whatever is blocking the delete.
				Kind:  extension.Transition,
				State: extension.Destroyed,
				Grants: []extension.Grant{
					extension.CloudMutate, extension.ClusterWrite,
				},
			},
			{
				// Re-count what should be gone. Read-only by construction: a gate
				// that could clean up would be able to make its own verdict true.
				Kind:   extension.Assertion,
				State:  extension.Destroyed,
				Grants: []extension.Grant{extension.CloudRead},
			},
		},
	}
}
