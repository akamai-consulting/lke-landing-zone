package assertplatform

// extension.go — `assert-platform` declares itself.
//
// FIFTEENTH EXTENSION, AND THE FIRST THAT IS PURELY ASSERTIONS — which it was
// when written, and is not any more. It began as four lanes observing a platform
// someone else built, each holding one read grant, and that is what an
// assertion-only extension is supposed to look like.
//
// It has since grown a `nudge-and-reap` TRANSITION (two lanes really do mutate;
// the capability layer surfaced it) and a `k8s-version` preflight holding two
// grants, one of which leaves the machine. TEN bindings now, and the header says
// so rather than leaving a reader to discover it in the declaration — the reason
// this extension was worth putting on the record survives the growth, but the
// count was evidence and evidence goes stale.
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
//	assertion:verified   "health-workflow"  [cluster-read]
//	assertion:verified   "argo-app"         [cluster-read]
//	assertion:verified   "argo-comparisons" [cluster-read]
//	assertion:verified   "overlay-applied"  [cluster-read]
//	assertion:configured "overlay-appliability" [cluster-read]
//	assertion:verified   "instance-custom"  [cluster-read]
//	transition:converged "nudge-and-reap"   [cluster-read cluster-write]
//	assertion:verified   "apl-deployed-version" [cluster-read]
//	assertion:configured "apl-version"      [read-repo]
//	assertion:configured "k8s-version"      [read-repo cloud-read]
//
// WHY THE LAST TWO BIND A DIFFERENT STATE. Three of these run a cluster and read
// what is there. The preflights do not. `apl-version` reads the instance's pinned
// apl-core chart version out of the SPEC FILE and compares it against the floor
// this llz supports; `k8s-version` reads the spec's cluster.k8sVersion and asks
// the LINODE ACCOUNT whether it may build it. No cluster is involved and none
// needs to exist — both are statements about how the instance is CONFIGURED,
// deliberately runnable before anything is provisioned, because refusing an
// unsupported chart after a 45-minute bootstrap (or an unbuildable k8s version
// fifteen minutes into a cluster apply) is the failure each exists to prevent.
//
// That is the same argument `token-inventory`'s validate-tokens lane makes, and
// the same shape: a preflight is not a gate just because it blocks. They read more
// than files, so they are assertions; they read them before provisioning, so they
// bind `configured`.
//
// TEN BINDINGS, NOT ONE. `guard-charts` established that a split needs divergent
// CAPABILITY rather than count, and three of these do hold identical grants — so
// on that rule alone they could collapse. They are named separately because their
// STATES differ (the two preflights are `configured`, the rest `verified`), and
// once the set is split at all, naming the siblings is what keeps the listing
// legible. Collapsing them would also hide that they fail independently: each is
// wired into a different CI lane and a reader of `llz extension list` should see
// ten things that can go red, not one.
//
// The grants are no longer uniform either, which is the other reason the split
// earns itself: `k8s-version` is the only lane here that leaves the machine, and
// `nudge-and-reap` the only one that writes.
//
// No ceiling change. Assertions may bind any state and `cluster-read`/`read-repo`
// are unrestricted.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "assert-platform",
		Short:  "assert the platform is what it claims: workflows run, apps sync, customisations land, and the pinned chart + k8s version are ones this instance can actually build",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Assertion,
				Name:   "health-workflow",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead},
			},
			{
				// THE OTHER HALF OF `apl-version`, and the half that reads a cluster.
				// `apl-version` resolves the chart version out of the SPEC, which on managed
				// App Platform cannot know the answer: Linode installs apl-core, `apl_enabled`
				// is a create-time boolean, and the Linode API carries no version field to
				// read. So the spec and the baseline can agree perfectly while the platform
				// runs something else — two consistent values that are not two correct ones.
				//
				// `verified`, not `configured`, and that is the whole distinction from its
				// sibling: this one observes a running platform and cannot be answered before
				// one exists.
				Kind:   extension.Assertion,
				Name:   "apl-deployed-version",
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
				// THE SWEEP FOR A COMPARISON THAT NEVER HAPPENED. `argo-app` waits for an
				// Application to APPEAR; this one asks, of every Application that exists,
				// whether Argo could compare it at all — a question whose answer is
				// invisible in sync.status, because a failed comparison leaves the previous
				// verdict standing.
				//
				// Its own binding rather than a second lane under `argo-app`: they fail
				// independently and in different lanes, and a reader of `llz extension list`
				// should see both things that can go red.
				Kind:   extension.Assertion,
				Name:   "argo-comparisons",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead},
			},
			{
				// THE GENERIC HALF OF appvalues.yaml's OWN RULE. That file requires every
				// entry to be backed by a gate that reads the consumer, and the rule has
				// been honoured one app at a time (`assert-loki` reads the running
				// ingester). This lane asks the same question for the mapped paths of every
				// app, and asks the second one an app-specific probe does not: when the
				// value is absent, is it absent because nothing delivered it, or because
				// the API server will never accept it?
				//
				// cluster-READ, and the dry run is why that is honest rather than
				// convenient: `patch --dry-run=server` sends the object for validation and
				// admission and persists nothing, which capability.Permits classifies as a
				// read. A gate that needed cluster-write to ask a question would be a gate
				// nobody could safely point at production — where this class of failure is
				// the only place it exists.
				Kind:   extension.Assertion,
				Name:   "overlay-applied",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead},
			},
			{
				// THE PR-TIME HALF OF overlay-applied's QUESTION, and the reason it is a
				// separate binding rather than a mode of that one: they run against
				// different clusters for different purposes. overlay-applied points at
				// PRODUCTION and asks whether a value landed. This points at a kind cluster
				// holding generated pre-overlay fixtures and asks whether the field map's
				// CreateOnly claim is TRUE — a question about this repo's code, answered by
				// an apiserver because no static reading of YAML can answer it.
				//
				// `configured` rather than `verified`, on apl-version's argument: it is a
				// statement about how the landing zone is CONFIGURED, made before any real
				// infrastructure exists, and deliberately runnable on a PR. Nothing about a
				// production platform is being observed.
				//
				// cluster-READ, like the lane it complements, and for the same reason:
				// `patch --dry-run=server` persists nothing and capability.Permits
				// classifies it as a read. The fixtures ARE a write, and they are applied
				// by the kind workflow's own kubectl rather than by this verb — which is
				// what keeps the grant honest instead of convenient.
				Kind:   extension.Assertion,
				Name:   "overlay-appliability",
				State:  extension.Configured,
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
			{
				// THE SECOND PREFLIGHT, AND THE FIRST THAT CANNOT ANSWER OFFLINE. It reads
				// the spec's cluster.k8sVersion (read-repo, like apl-version) and then asks
				// the LINODE ACCOUNT whether it may build that version (cloud-read) —
				// because LKE-Enterprise availability is per-account, so the spec alone
				// cannot settle it and neither can a release note.
				//
				// `configured` for the same reason apl-version is: it is a statement about
				// how the instance is CONFIGURED, made before any cluster exists, precisely
				// so a bad pin does not surface fifteen minutes into an apply that has
				// already built a VPC, object storage and databases.
				//
				// cloud-READ and nothing else. It looks at a version catalog; a lane that
				// could also delete the cluster it is checking is the anomaly
				// TestEveryLaneOnlyObserves exists to refuse.
				Kind:   extension.Assertion,
				Name:   "k8s-version",
				State:  extension.Configured,
				Grants: []extension.Grant{extension.ReadRepo, extension.CloudRead},
			},
		},
	}
}

// k8sVersionBinding is the cloud-read binding the version preflight reads the
// account through. Named rather than indexed, for the reason MutatingBinding is:
// adding an assertion must not silently shift which grants a handle is built from.
func k8sVersionBinding() extension.Binding {
	return Extension().MustBinding("k8s-version")
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
