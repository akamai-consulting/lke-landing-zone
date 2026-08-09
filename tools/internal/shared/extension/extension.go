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
// REQUIRES IS THE PRECONDITION AXIS, and it is the third word this model has
// gained. State says what a binding ESTABLISHES; Requires says what must ALREADY
// HOLD for it to run. Until now only the first was expressible, and three
// extractions in a row shipped a declaration that was true about the first while
// silently dropping the second.
//
// THE SHAPE IS A MUTATING ACTION WHOSE PRECONDITION IS A STATE RATHER THAN WHOSE
// EFFECT IS THAT STATE. `wedge-gameday` refuses to start unless the cluster is
// already Healthy; `rotate-admin` refuses to rotate an unseeded path;
// `bao-breakglass` restores root access to a platform that is up. None of the
// three moves the platform anywhere, and all three need it to be somewhere.
//
// WHY NOT A FIFTH BINDING KIND, which is the obvious reading and the one the
// first two cases were nearly argued into. `wedge-gameday` wrote the refutation
// while declaring itself: what these want "is not a new binding kind but a way to
// say 'this is a CHECK, it must mutate to run, and it requires state X rather than
// establishing it'. A fifth kind bolted on now would have answered the kind
// question and left the state question exactly where it is." The kind was never
// wrong — all three really are transitions, they really do mutate. The STATE was
// carrying two meanings and could only ever express one.
//
// WHY NOT LET Transition REACH `operating` INSTEAD, the other obvious fix.
// Because the restriction has content worth keeping: `operating` is a condition
// that holds rather than a place you move to, and letting a transition target it
// would let something claim to move the platform TO `operating`, which is the
// exact thing the rule exists to prevent. Requires buys the accuracy without
// spending the rule.
//
// IT ALSO RESOLVES A DISAGREEMENT BETWEEN THE TWO CEILING TABLES, which is the
// sharpest evidence that the gap was real rather than cosmetic. grantStates puts
// `operating` in the secret-custody row explicitly FOR rotation, while
// bindableStates forbids a transition there — so a scheduled rotation was a
// binding each table expected the other to be describing. `rotate-admin` ended up
// at `seeded`, the one state both tables allow, which its own note called out as
// the wrong home.
//
// THE GRANT CHECK RUNS AT BOTH STATES, never at Requires alone. Checking only the
// precondition would have been the natural reading and is a quiet widening: a
// binding could then ask at `operating` for something its declared State forbids.
// Both is strictly tighter than what shipped before this field existed, and all
// three cases pass it unchanged.
type Binding struct {
	Kind     BindingKind
	Name     string
	State    State
	Requires State
	Grants   []Grant
}

func (b Binding) String() string {
	s := string(b.Kind) + ":" + string(b.State)
	if b.Name != "" {
		s += "/" + b.Name
	}
	// Spelled out rather than punctuated. This string appears in every validation
	// error, and a reader meeting `transition:seeded<operating` for the first time
	// has to guess which side is the precondition; the two states are the whole
	// point of the field and are worth six characters to keep unambiguous.
	if b.Requires != "" {
		s += " (requires " + string(b.Requires) + ")"
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
	WriteRepo     Grant = "write-repo"     // create, rewrite or delete the instance repo's files
	ClusterWrite  Grant = "cluster-write"  // mutate cluster state
	CloudMutate   Grant = "cloud-mutate"   // create/destroy cloud resources
	SecretRead    Grant = "secret-read"    // read credential material or its metadata
	SecretCustody Grant = "secret-custody" // place or store credential material
	OwnPaths      Grant = "own-paths"      // own instance files against `copier update`
)

// ADDING write-repo WAS THE TWENTY-EIGHTH EXTENSION'S FINDING, and it is the
// SECOND word this vocabulary has gained — the first since secret-custody was
// split. It took four cases, and three refusals, to get here.
//
// THE THREE REFUSALS ARE THE POINT. `llz ci gen-toc`, `guard-docs` and
// `promote-pipeline` each write the operator's repo and each declared `read-repo`,
// because in all three the write could be lifted OUT of the extension and left in
// package main: the package renders bytes, main calls os.WriteFile. Each time the
// catalog recorded the gap and refused to invent a word, on the stated grounds
// that two cases say the vocabulary has a hole and do not say what SHAPE it is.
//
// `deliver-docs` is the case where that answer stops working, and the difference
// is structural rather than a matter of degree. It does not render bytes for
// someone else to write — it PRUNES A DIRECTORY and rewrites links in place,
// deciding per file, mid-walk, from the file's own inode identity and whether the
// template owns its path. Lifting the writes out means either buffering every
// rewritten file to hand back, or passing main a callback that writes — which is
// the write happening inside the package with extra indirection and a worse
// boundary. The declaration was not INCOMPLETE, which a file split fixes; it was
// IMPOSSIBLE, which is this model's stated bar for a new word.
//
// AND THE SHAPE IS NOW KNOWN, which is what the earlier refusals were waiting for.
// The question they left open was which writes count: the operator's repo, or a
// build artifact, or a temp file. All four cases answer it the same way — they
// write files an operator has checked in and will read a diff of. So write-repo
// means the INSTANCE REPO'S TRACKED FILES, and a temp dir or a render output under
// .instance-test needs no grant, the same way reading /tmp needs no read-repo.
//
// It is NOT own-paths, and the two are easy to confuse. own-paths is a FENCE —
// "copier must not render these bytes" — and says nothing about who writes them.
// write-repo is a PERMIT and says nothing about copier. deliver-docs holds the
// permit and not the fence: it rewrites files copier itself rendered, and wants
// them re-rendered on the next update.

// SPLITTING secret-custody WAS THE THIRTEENTH EXTENSION'S FINDING, and the first
// that could not be fixed by adding a table row.
//
// This constant used to be one word documented as "read OR WRITE credential
// material" — the ambiguity was written down and then relied on. Three extractions
// pushed on it in a row:
//
//   - cluster-access WRITES a cluster-admin kubeconfig to disk. Custody.
//   - health-sla READS `updated_time` off a KV entry using the OpenBao root
//     token. Declared custody, and the declaration said so under protest.
//   - token-inventory READS every pipeline credential and probes it. Mutates
//     nothing.
//
// The third one broke it outright. `validate-tokens` blocks the pipeline before
// anything provisions, so it is a gate or an assertion; a gate permits read-repo
// alone, and an assertion permits READ GRANTS ONLY — and secret-custody was not
// one, because it was half a write grant. The check was therefore INEXPRESSIBLE:
// not mis-described, not over-granted, simply impossible to declare honestly.
//
// So: reading a credential is now secret-read (read-only, an assertion may hold
// it), and PLACING one is secret-custody (mutating, still bound by grantStates).
// The distinction is the one the reviewer actually cares about — "this could leak
// a secret" versus "this decides what the secret IS" — and it was always the one
// the single word could not draw.

// Grants returns the closed vocabulary, ordered least to most dangerous.
func Grants() []Grant {
	return []Grant{ReadRepo, CloudRead, ClusterRead, SecretRead, WriteRepo, ClusterWrite, CloudMutate, SecretCustody, OwnPaths}
}

// readOnly are the grants that observe without changing anything. secret-read is
// here and secret-custody is not: that is the whole content of the split.
var readOnly = map[Grant]bool{ReadRepo: true, CloudRead: true, ClusterRead: true, SecretRead: true}

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

	// Component names the `spec.components` toggle whose enablement decides this
	// extension's. Empty means enablement does not follow a component.
	//
	// THE SPEC ALREADY HAD ENABLEMENT AND THIS REUSES IT rather than inventing a
	// parallel `extensions:` map for an operator to keep in step. Components carry
	// Mandatory, DependsOn and a tri-state toggle that Defaults() fills; an
	// extension that tracks a platform feature is enabled exactly when the feature
	// is. obj-encryption said so in prose long before this field existed — "once,
	// when spec.components.objProxy is enabled" — and that comment was the only
	// thing connecting the two.
	//
	// A STRING, NOT A TYPED REFERENCE, because this package must not import
	// clusterspec: it is the model every extension depends on, and
	// TestDeclarationModelStaysDependencyFree pins that it imports nothing but
	// strings. Resolution lives in the registry, which may import both — and the
	// registry is also where a name that matches no component is caught, since only
	// something holding the component list can know.
	//
	// IT IS EXTENSION-SCOPED, WHICH IS THE MISTAKE Grants WAS CORRECTED FROM, AND
	// THERE IS ONE CASE THAT PROVES IT — which is one short of this model's bar.
	//
	// `assert-secrets` carries four named bindings, and each belongs to a DIFFERENT
	// component: eso-roundtrip follows externalSecrets, openbao-audit follows
	// openbao, broad-pat-drill follows broadPatRotator, and rotation-health follows
	// none of them (it gates every credential credpaths declares). A single
	// Component here can only be one of those, so the extension carries none and an
	// instance that dropped externalSecrets still runs the ESO round-trip.
	//
	// THE FIX IS NOT A SLICE. `[]string` would ask a question with no good answer —
	// all of them off, or any? — and both readings are wrong for assert-secrets,
	// whose four bindings want four independent answers. The shape is per-BINDING,
	// which is exactly the argument made for grants twenty lines up: every rule
	// about a component is really a rule about the binding that follows it, so
	// extension-scoped components cannot be reconciled with multi-binding
	// extensions.
	//
	// AND `openbao` FOUND THE SAME AXIS FROM THE OTHER SIDE, independently, which is
	// the part worth reading. Its Incomplete note says "ENABLEMENT IS PER-EXTENSION,
	// NOT PER-BINDING": `openbao-peer-ca` only matters for an HA PAIR and shipped
	// `Always: false`, while its siblings are unconditional — merging forced one
	// value and the honest one was wrong. That note refuses a `Binding.Always` on
	// the grounds that it is "STILL ONE CASE", and ends "this note stays until a
	// second case says what shape the answer is."
	//
	// assert-secrets is a second case on the same axis and it says the shape is NOT
	// what openbao guessed. peer-ca wants a PREDICATE — it runs when the deployment
	// is an HA pair, a topology fact read at run time, not a toggle anyone sets.
	// assert-secrets wants a per-binding COMPONENT REFERENCE, which is a static
	// value and much smaller. Two cases, two shapes, so what they establish
	// together is that the AXIS is real (enablement belongs on the binding) and not
	// yet what the field is. A `Binding.Component` would serve assert-secrets and
	// still miss peer-ca, because there is no component called `ha`.
	//
	// NOTHING IS INVENTED HERE. The two remaining candidates were checked and are
	// not cases: `credential-pat` has a single binding (so its link is a
	// whole-extension decision), and `health-sla`'s two invariants are both
	// cross-cutting — neither follows a component at all. The reconciler lanes
	// already carry the binding name per-binding resolution would need (see
	// LaneDecl), so the plumbing is not what is missing; the argument is.
	//
	// EMPTY IS NOT "ALWAYS ON". Four of the seven opt-in extensions have no
	// component and should not be given one: import-brownfield is a one-time
	// adoption path, wedge-gameday and dev-mutation-testing are not about the
	// platform at all, and release-publish runs template-repo-side. Enablement for
	// those is a question this field does not answer, and answering it here would
	// mean inventing a component that exists only to be a checkbox.
	Component string
	// Bindings is where it attaches and, per binding, what it may touch. At least
	// one.
	Bindings []Binding

	// Incomplete names what this extension does NOT yet declare, and is empty for
	// an extension that declares its whole surface.
	//
	// IT EXISTS BECAUSE TWO EXTRACTIONS ARRIVED PARTIAL AND THE MODEL COULD NOT SAY
	// SO. `reconcile-actions` declares four invariants while four more of its lanes
	// are still in package main; `template-sustain` declares the half that does not
	// touch `.template-manifest`, which ADR 0014 pins as permanently core. Both
	// read as COMPLETE — nothing distinguished "has four bindings" from "has eight,
	// four of which have not moved" — and an extension that silently under-declares
	// its own surface is the same failure shape as PR #15's ban-by-omission: the
	// reader cannot tell what is missing.
	//
	// Added on the SECOND case, not the first, which is the rule this model has
	// been following for every vocabulary change (see grantStates' cloud-mutate
	// row). One occurrence is an anecdote; two independent ones with different
	// causes — a sibling extension's territory, and a core-by-construction file —
	// is a shape.
	//
	// IT IS PROSE, NOT A SCHEMA, and deliberately: what is missing is a sentence
	// about code that has not moved, and there is nothing yet to validate it
	// against. Validate() only checks that the strings are non-empty, so the field
	// cannot become a silent `[]string{""}`. When the loader exists and can compare
	// a declaration against what is registered, this is where the discrepancy gets
	// reported.
	Incomplete []string
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
