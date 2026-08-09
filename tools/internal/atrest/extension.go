package atrest

// extension.go — `posture-at-rest` declares itself.
//
// THIRD EXTENSION, AND THE FIRST THAT IS NOT A GATE. The first two were both
// `gate:scaffolded[read-repo]`, which left three of four binding kinds and nine of
// ten states unexercised — the model's interesting half untested by the things
// declared against it. This one was picked to move that, and the pick was made
// from the catalog's own classification rather than from what was convenient.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `posture-at-rest` declaration.
//
// `invariant:operating[read-repo]` — the first invariant, and the first binding at
// a state other than `scaffolded`.
//
// WHY AN INVARIANT AND NOT A GATE, WHEN THE CODE IS A STATIC FILE SCAN. This is
// the question the third extension was supposed to force, and the answer is not
// "what does the code do" but "what is being claimed":
//
//   - A GATE claims something about a CHANGE: run it before the state is
//     attempted, over the files in hand, and a breach means "do not proceed".
//     `guard-budgets` and `guard-docs` are that — a budget is a fact about the
//     commit, and a broken doc link is a defect in the diff.
//   - An INVARIANT claims something about the RUNNING SYSTEM that must not stop
//     being true. "Every Terraform-declared resource is encrypted at rest" is
//     that, and the ADR says why it cannot be anything else: **encryption is
//     decided at CREATE and is immutable afterwards**. There is no remediation for
//     a resource that came up unencrypted, only a rebuild — so the property has to
//     hold continuously across every future apply, not merely at the moment
//     someone happened to run the checker.
//
// The catalog classed this an invariant and it was right, but for a reason it did
// not state: the discriminator is the CLAIM'S LIFETIME, not the implementation.
// That distinction was invisible while every declared extension was a gate, and it
// is the first thing this model has said that the code alone does not.
//
// WHAT IT COSTS TO BE HONEST ABOUT THAT. An invariant may attach only to
// `operating`, and this scan runs in template-repo CI long before anything is
// operating. So the declaration is a claim the driver cannot yet honour: nothing
// schedules an invariant, and when something does, this one needs to run against
// the tree an instance is CURRENTLY built from rather than the one in front of it.
// Recorded here rather than smoothed over — declaring `gate` instead would have
// been the convenient lie, and it would have made the model agree with itself by
// making the model say nothing.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "posture-at-rest",
		Short:  "every Terraform-declared resource is encrypted at rest, and stays that way",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Invariant,
			State:  extension.Operating,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
