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

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

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

// cloudBinding returns the binding a Linode call runs under: the destroying
// transition when the call may actually delete, the read-only assertion when it
// cannot.
//
// ────────────────────────────────────────────────────────────────────────────
// THE SELECTION IS A RUNTIME VALUE, WHICH IS NEW, AND IT MAY ONLY NARROW.
//
// Everywhere else in this tree a call site belongs to one binding and you can
// read the declaration to know what that code may do. Here four of the six sites
// are gated on `--yes`/`--dry-run`: the reapers build one client and then either
// delete through it or print "would DELETE" and return, and which of those
// happens is not knowable from the call site.
//
// The rule that keeps the model's guarantee intact is that `mutating` can only
// ever pick the WEAKER binding. The maximum a code path may do is still static
// and still readable from the declaration — the transition's cloud-mutate — and
// the flag can subtract from it but never add. A reader asking "what is the worst
// this can do" gets the same answer as before; a reader asking "what can it do
// right now" gets a better one.
//
// WHAT IT BUYS, and it is specific rather than tidiness. `--dry-run` not deleting
// is currently enforced by ONE early `return` inside Deleter's closure. That
// closure is the only thing between a dry run and a destroyed cluster. Selecting
// the read binding puts a second, independent refusal at the transport, so a bug
// in that `if` is caught by the fence rather than by an operator reading the
// aftermath.
//
// EARLIER NOTES IN THIS TREE SAID THIS WAS BLOCKED, and they were wrong on the
// facts. They claimed `runCIReapVolumes(…, requireEmpty)` served both bindings
// through one function. It does not: `--require-empty` adds a verification pass
// AFTER the sweep and never suppresses a delete, and the assertion has its own
// entry point (RunAssertNoOrphans) whose client comes from a Deps seam. The six
// construction sites are all transition-side. What is genuinely runtime-varying
// is only whether a given run deletes at all — and EVERY site now narrows on
// that. Two did not: RunForceDelete and RunDeleteVPC passed a hardcoded `true`,
// so the two most destructive verbs in the package were the only two holding a
// DELETE-capable transport through a dry run. That is what the read binding here
// exists to prevent, and it was absent from exactly the paths that most need it.
// ────────────────────────────────────────────────────────────────────────────
func cloudBinding(mutating bool) extension.Binding {
	want := extension.Assertion
	if mutating {
		want = extension.Transition
	}
	for _, b := range Extension().Bindings {
		if b.Kind == want {
			return b
		}
	}
	panic("teardown: no " + string(want) + " binding — the Linode client is built " +
		"from one, so its absence is a wiring bug")
}
