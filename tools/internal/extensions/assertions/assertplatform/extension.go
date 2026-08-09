package assertplatform

// extension.go — `assert-platform` declares itself.
//
// FIFTEENTH EXTENSION, AND THE FIRST THAT IS PURELY ASSERTIONS. Four lanes that
// observe a platform someone else built and report whether it is what it claims
// to be. Nothing here mutates, and the declaration is four bindings holding one
// read grant each — which is what an assertion-only extension is supposed to look
// like, and worth having one of on the record now that the mutating shapes are
// all exercised.
//
// THE CATALOG NAMED FIVE FILES; FOUR BELONG. `ci_assert_image_fresh.go` stayed in
// package main: its closure is the TEMPLATE-PIN machinery (assertPinCoherence,
// pinnedTemplateRef, resolveTemplateCommit), not platform health. It asserts that
// an instance's pinned template ref and its images agree — a `template-sustain`
// question wearing an `assert-` filename. Fourth time the catalog's file list has
// been wrong, and the fourth time for the same reason: it grouped by name.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `assert-platform` declaration.
//
//	assertion:verified "health-workflow"  [cluster-read]
//	assertion:verified "argo-app"         [cluster-read]
//	assertion:verified "instance-custom"  [cluster-read]
//	assertion:configured "apl-version"    [read-repo]
//
// WHY THE LAST ONE BINDS A DIFFERENT STATE. Three of these run a cluster and read
// what is there. `assert-apl-version` does not: it reads the instance's pinned
// apl-core chart version out of the SPEC FILE and compares it against the floor
// this llz supports. There is no cluster involved and none needs to exist — it is
// a statement about how the instance is CONFIGURED, and it is deliberately
// runnable before anything is provisioned, because refusing an unsupported chart
// after a 45-minute bootstrap is the failure it exists to prevent.
//
// That is the same argument `token-inventory`'s validate-tokens lane makes, and
// the same shape: a preflight is not a gate just because it blocks. It reads more
// than files, so it is an assertion; it reads them before provisioning, so it
// binds `configured`.
//
// FOUR BINDINGS, NOT ONE. `guard-charts` established that a split needs divergent
// CAPABILITY rather than count, and three of these do hold identical grants — so
// on that rule alone they could collapse. They are named separately because their
// STATES differ (apl-version is `configured`, the rest are `verified`), and once
// the set is split at all, naming the three siblings is what keeps the listing
// legible. Collapsing the three would also hide that they fail independently:
// each is wired into a different CI lane and a reader of `llz extension list`
// should see four things that can go red, not one.
//
// No ceiling change. Assertions may bind any state and `cluster-read`/`read-repo`
// are unrestricted.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "assert-platform",
		Short:  "assert the platform is what it claims: workflows run, apps sync, customisations land, the chart is supported",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Assertion,
				Name:   "health-workflow",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead},
			},
			{
				Kind:   extension.Assertion,
				Name:   "argo-app",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead},
			},
			{
				Kind:   extension.Assertion,
				Name:   "instance-custom",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead},
			},
			{
				// THE MUTATING HALF, AND IT WAS UNDECLARED UNTIL THE CAPABILITY LAYER
				// ASKED. Two of the assertions above mutate: argo-app forces a hard
				// refresh on a wedged parent Application, and health-workflow reaps its
				// own prior probe Workflows before submitting a new one. Both were going
				// through a general exec seam, so `assertion:verified[cluster-read]` and
				// a mutating binding produced identical behaviour and nothing could tell
				// them apart.
				//
				// It is a SEPARATE BINDING rather than cluster-write added to those
				// assertions, because Validate refuses a mutating grant on an assertion —
				// "if it must mutate, declare the mutating half as its own transition
				// binding". That rule was written for exactly this and had never been
				// exercised against a real violation.
				//
				// `converged` rather than `verified`: a transition cannot target
				// `verified` (it is the conclusion of assertions, not a place you move
				// to), and both mutations are nudges that drive the platform TOWARD
				// convergence — the refresh makes Argo re-fetch, the reap clears a Failed
				// Workflow that would otherwise be read as a live failure.
				Kind:   extension.Transition,
				Name:   "nudge-and-reap",
				State:  extension.Converged,
				Grants: []extension.Grant{extension.ClusterRead, extension.ClusterWrite},
			},
			{
				Kind:   extension.Assertion,
				Name:   "apl-version",
				State:  extension.Configured,
				Grants: []extension.Grant{extension.ReadRepo},
			},
		},
	}
}

// MutatingBinding is the `nudge-and-reap` transition — the only binding here that
// may write. Callers name it explicitly rather than indexing Bindings, so that
// adding an assertion cannot silently shift which grants the writer is built from.
func MutatingBinding() extension.Binding {
	// PANICS RATHER THAN RETURNING A ZERO BINDING. It used to do the latter, and a
	// zero Binding declares no grants — so a rename would have handed the caller a
	// refusing Writer and surfaced as a permission error naming a grant nobody
	// forgot. That is the failure two assert lanes actually shipped with, from the
	// positional form of the same mistake. Thirty-two sibling accessors panic; these
	// two were the exceptions, and they are the two feeding a MUTATING handle.
	return Extension().MustBinding("nudge-and-reap")
}
