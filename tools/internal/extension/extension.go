package extension

import "strings"

// State is a position in the platform lifecycle. The names are the ones the
// catalog (docs/designs/internal-extensions.md) grouped all 214 files under, and
// they are the vocabulary an extension uses to say WHERE it attaches rather than
// WHAT it is. That substitution is the point: PR #15's `kind: check|tool` asked
// what an extension is, which made `→ seeded` — 6,874 lines of credential and
// secret provisioning — structurally inexpressible, because there was no skeleton
// for it and the menu of skeletons was the ceiling.
type State string

const (
	Scaffolded  State = "scaffolded"  // the instance repo exists and is rendered
	Configured  State = "configured"  // its inputs resolve — env topology, tokens, readiness
	Provisioned State = "provisioned" // cloud substrate exists — cluster, VPC, firewall
	Seeded      State = "seeded"      // credentials and secret material are in place
	Converged   State = "converged"   // the platform's declared components are reconciled
	Verified    State = "verified"    // assertions over a running platform hold
	Operating   State = "operating"   // steady state; invariants hold continuously
	Promoted    State = "promoted"    // a change moved between environments
	Upgraded    State = "upgraded"    // the instance took a newer template/binary
	Destroyed   State = "destroyed"   // the substrate is gone and nothing leaked
)

// lifecycle is the ordered spine. promoted/upgraded/destroyed sit outside it —
// they are transitions an operating instance takes repeatedly, not stations on
// the way up.
//
// FIVE OF THE SEVEN ARE ENTERED BY ACTING; the last two are not, and that is the
// model's shape rather than a gap in it. `verified` is the conclusion drawn when
// the assertions required at that state hold, and `operating` is the condition of
// continuing to satisfy its invariants — neither is somewhere an extension moves
// the platform to, which is why bindableStates gives Transition no way to target
// them. What follows is that ADVANCING past `converged` is the driver's job, not
// an extension's: the driver evaluates the required set and names the state. That
// division has no code yet (this package is wired to nothing) and it is the first
// thing the driver slice has to get right, because if extensions could declare
// "verified reached" the core would no longer own what success means.
var lifecycle = []State{Scaffolded, Configured, Provisioned, Seeded, Converged, Verified, Operating}

var recurring = []State{Promoted, Upgraded, Destroyed}

// States returns every valid state, spine first. Callers get a stable order so
// help text and error messages do not depend on map iteration.
func States() []State {
	out := make([]State, 0, len(lifecycle)+len(recurring))
	out = append(out, lifecycle...)
	return append(out, recurring...)
}

func validState(s State) bool {
	for _, k := range States() {
		if k == s {
			return true
		}
	}
	return false
}

// BindingKind is HOW an extension attaches to a state. The four kinds are the
// distinct shapes the catalog found; nothing in package main needed a fifth.
type BindingKind string

const (
	// Transition ACTS to move the platform into its state. The bulk of the
	// catalog: bootstrap, seed, converge, teardown.
	Transition BindingKind = "transition"
	// Assertion contributes evidence that a state HOLDS. The core owns which
	// assertions are required; an extension only contributes one.
	Assertion BindingKind = "assertion"
	// Invariant must hold continuously while the platform is in its state. The
	// binding today's design has no room for, and 4,283 lines depend on it.
	Invariant BindingKind = "invariant"
	// Gate runs BEFORE a state is attempted, over files alone — findings out, no
	// cluster, no credential.
	Gate BindingKind = "gate"
)

// Binding is one attachment: a kind, the state it attaches to, and the grants
// THAT ATTACHMENT needs. An extension may carry several — the catalog's strongest
// structural signal is that a capability and its assertion (harbor-provisioner ↔
// assert-registry, database-provisioner ↔ assert-database) enable and disable
// together, which argues for one extension holding both bindings rather than two
// that must be kept in step by hand.
//
// GRANTS BELONG HERE, NOT ON THE EXTENSION. Every rule about a grant is really a
// rule about a binding ("a gate may only read the repo"), so extension-scoped
// grants cannot be reconciled with multi-binding extensions. Scoping them wrongly
// yielded a one-line bypass and an unsatisfiable pair, both pinned as regressions
// in extension_test.go; docs/designs/internal-extension-model.md explains them.
// Each binding is judged on what it declares, and lends nothing to its siblings.
//
// NAME DISAMBIGUATES REPEATED ATTACHMENTS, and exists because the same reasoning
// that moved grants onto Binding applies one level further down. `operating` is
// the only state an invariant may attach to, so without a name an extension could
// hold exactly one invariant — and reconcile-actions is SEVEN of them (ES-store
// recovery, OpenBao, tokens, apl-overlay, argo-nudge, sc-demote,
// linode-token-wait) whose needs genuinely differ: the token restorers place
// credential material, sc-demote only writes to the cluster. Collapsed into one
// binding their grants widen to the union, which is precisely the over-granting
// that scoping grants per binding was introduced to prevent. Optional: a single
// attachment needs no name, and two of the same kind:state do.
type Binding struct {
	Kind   BindingKind
	Name   string
	State  State
	Grants []Grant
}

func (b Binding) String() string {
	s := string(b.Kind) + ":" + string(b.State)
	if b.Name != "" {
		s += "/" + b.Name
	}
	if len(b.Grants) > 0 {
		s += "[" + grantList(b.Grants) + "]"
	}
	return s
}

// Grant is a capability a binding declares it needs. Grants replace PR #15's
// `kind: check|tool` ceiling: instead of a closed menu of shapes an extension may
// take, it declares what it TOUCHES and the validator decides whether its
// bindings permit that.
//
// ON THE CATALOG'S GRANT DISTRIBUTION — no grant held by a majority of the 57
// candidates — READ IT AS A DESIGN INTUITION, NOT A MEASUREMENT. The assignments
// were authored in the same pass that invented this vocabulary, so the spread
// reports the author's judgement rather than an independent property of package
// main. It is a reason to think the axis is discriminating; it is not evidence,
// and it cannot become evidence until extensions declare their own grants and the
// distribution is observed instead of assigned. The same caution applies to
// "nothing in package main needed a fifth binding kind" on BindingKind: the
// catalog was built with four in mind.
type Grant string

const (
	ReadRepo      Grant = "read-repo"      // read the instance repo's files
	CloudRead     Grant = "cloud-read"     // read cloud APIs
	ClusterRead   Grant = "cluster-read"   // read cluster state
	ClusterWrite  Grant = "cluster-write"  // mutate cluster state
	CloudMutate   Grant = "cloud-mutate"   // create/destroy cloud resources
	SecretCustody Grant = "secret-custody" // read or write credential material
	OwnPaths      Grant = "own-paths"      // own instance files against `copier update`
)

// Grants returns the closed vocabulary, ordered least to most dangerous.
func Grants() []Grant {
	return []Grant{ReadRepo, CloudRead, ClusterRead, ClusterWrite, CloudMutate, SecretCustody, OwnPaths}
}

// readOnly are the grants that observe without changing anything.
var readOnly = map[Grant]bool{ReadRepo: true, CloudRead: true, ClusterRead: true}

func validGrant(g Grant) bool {
	for _, k := range Grants() {
		if k == g {
			return true
		}
	}
	return false
}

// Extension is the declaration. It is deliberately inert — identity, where it
// attaches, and what it may touch. No action, no files, no manifest: see the
// package doc on why the action ABI is absent until `converge` and
// `import-brownfield` force its shape.
type Extension struct {
	// Name is the stable identifier, kebab-case (guard-budgets, assert-storage).
	Name string
	// Short is a one-line summary for `llz extension list`.
	Short string
	// Always means the extension ships ENABLED on every instance. 41 of the
	// catalog's 57 are always, against 16 opt-in — universality turned out not to
	// be the thing that distinguishes an extension from core, which is why this is
	// a registry fact recorded here rather than a mechanism that gates what may
	// become one.
	//
	// IT IS A DEFAULT, NOT A CONSTANT. The assert lanes are the case that settles
	// this: `llz ci assert-suite` is called from three places in instance-template
	// (bootstrap unconditionally, cluster-health as a six-lane subset, and
	// scheduled-checks), so an instance with no object storage has to be able to
	// turn assert-objstore off in its own configuration rather than by taking a
	// different build. The registry that reads this field must therefore let an
	// instance override it in both directions.
	//
	// NOTHING VALIDATES IT, DELIBERATELY. There is no rule an always-enabled
	// extension must satisfy that an opt-in one need not; inventing one to give
	// the field "teeth" would be a rule that exists to justify a field rather than
	// to catch a mistake. It is declaration data the registry reads when it
	// decides the default enabled set.
	Always bool
	// Bindings is where it attaches and, per binding, what it may touch. At least
	// one.
	Bindings []Binding
}

// Grants is the union of every binding's grants — "what does this extension
// touch?", which is the question the catalog's distribution answers and the one a
// reviewer asks. Derived rather than declared, so it can never disagree with the
// bindings it summarises. Returned in vocabulary order (least to most dangerous)
// so output is stable.
func (e Extension) Grants() []Grant {
	seen := map[Grant]bool{}
	for _, b := range e.Bindings {
		for _, g := range b.Grants {
			seen[g] = true
		}
	}
	var out []Grant
	for _, g := range Grants() {
		if seen[g] {
			out = append(out, g)
		}
	}
	return out
}

// HasGrant reports whether any binding declares g.
func (e Extension) HasGrant(g Grant) bool {
	for _, b := range e.Bindings {
		for _, have := range b.Grants {
			if have == g {
				return true
			}
		}
	}
	return false
}

// Binds reports whether the extension carries a binding of the given kind.
func (e Extension) Binds(k BindingKind) bool {
	for _, b := range e.Bindings {
		if b.Kind == k {
			return true
		}
	}
	return false
}

func grantList(gs []Grant) string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = string(g)
	}
	return strings.Join(out, ", ")
}
