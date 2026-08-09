package extension

import (
	"fmt"
	"sort"
	"strings"
)

// nameRE-equivalent, hand-rolled to keep the package dependency-free: kebab-case,
// starting and ending alphanumeric.
func validName(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") {
		return false
	}
	if strings.Contains(s, "--") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// bindableStates says which states each binding kind may attach to. This table is
// the model's shape in one place — a new state or a new kind is an edit here, not
// a new rule scattered through Validate.
//
// Transition targets every state EXCEPT verified and operating, because neither
// is reached by acting: `verified` is the conclusion of assertions, and
// `operating` is a condition that holds rather than a place you move to.
//
// Assertion targets EVERY state, including the three recurring ones. Two reasons,
// both concrete:
//
//   - A state that can be entered can be wrong about having been entered, and the
//     recurring states are where that costs money. `assert-no-orphans`
//     (ci_teardown.go) is the assertion that `destroyed` actually holds; leaving
//     it inexpressible would have been the sharpest possible case of the model
//     failing the code it was derived from. Asserting `upgraded` (template drift)
//     and `promoted` follows the same way.
//   - It need not target `verified` alone. internal/health is the precedent: 1,164
//     logic lines of pure classification (argo.go, certs.go, matchers.go) that
//     ci_health.go's own header calls "the tested internal/health predicate", with
//     that command reduced to the kubectl orchestration feeding it. An
//     already-separated predicate for `converged` is exactly an assertion at a
//     non-verified state, and config-readiness is the same shape for `configured`.
//     A rule admitting only `verified` would have no room for either.
var bindableStates = map[BindingKind][]State{
	Transition: {Scaffolded, Configured, Provisioned, Seeded, Converged, Promoted, Upgraded, Destroyed},
	Assertion:  {Scaffolded, Configured, Provisioned, Seeded, Converged, Verified, Operating, Promoted, Upgraded, Destroyed},
	Invariant:  {Operating},
	Gate:       {Scaffolded, Configured},
}

// grantStates is the second half of the ceiling: WHERE a dangerous capability may
// be asked for. Without it the model requires secret-custody in one place and
// forbids it in none — a transition to `scaffolded` could declare it and validate
// clean — which leaves "declare what you touch and be judged on it" true only for
// the two kinds that carry a blanket rule (gate, assertion), covering 13 of the
// catalog's 57 declarations while the 44 transitions and invariants go unchecked.
//
// Only the mutating grants are listed; the read grants are unrestricted, because
// reading is not what the reviewer is being protected from. The states come from
// where the catalog actually places each grant, so this table is JUDGEMENT
// TRANSCRIBED, not a derived fact — it is the most likely thing here to need a row
// added, and adding one should be an argued change rather than a quiet widening.
var grantStates = map[Grant][]State{
	// SECOND WIDENING. `provisioned` was added for `cluster-access`, which fetches
	// the cluster-admin kubeconfig. The row had only ever been shown credentials the
	// platform MINTS (seeding) or REPLACES (rotation), both of which happen to a
	// cluster that already works — so it encoded "custody begins once there is a
	// platform to hold it". The bootstrap credential breaks that: the cloud issues
	// it at provisioning time, and holding it is the PRECONDITION for seeding, not a
	// consequence of it. A table that cannot express the first credential in the
	// system's life is describing the middle of the story only.
	//
	// Note this widening is not symmetric with the first one. cloud-mutate gained
	// `operating` at the END of the lifecycle (reconciler lanes, which keep running);
	// this gains one at the START. Both were found the same way — by an extraction
	// of code that already shipped — which is the argument for extracting the
	// expensive capabilities before trusting the ceiling, not after.
	SecretCustody: {Provisioned, Seeded, Operating},
	// THIRD WIDENING, and the one that took longest to earn: `configured`, added
	// for `chart-publish`'s --publish-if-missing dispatch.
	//
	// env-topology (twenty-first extraction) wrote this exact binding for
	// branchpolicy.go's PUT against GitHub's deployment-branch-policy API, ran
	// Validate(), was refused by this row, and moved the file back to package main
	// rather than widen on one case. It predicted the second case would be `llz
	// tokens`; it turned out to be a `gh workflow run` dispatch. That the
	// prediction missed is itself evidence the shape is general rather than
	// specific to branch policies.
	//
	// THE ARGUMENT THE ROW WAS MISSING. The other five states are where a LINODE
	// cloud exists to mutate, and `configured` was read as a purely local moment —
	// resolve some inputs, touch nothing. It is not. GitHub is configured before
	// Linode is provisioned, and "its inputs resolve" can require CREATING an
	// input rather than merely reading it: a pinned chart the registry never
	// received, and a branch policy nobody locked, are both inputs that do not
	// resolve until something writes.
	//
	// `scaffolded` is still absent and should stay absent — at scaffolding there is
	// no configured credential to mutate anything WITH, which is the same reasoning
	// that keeps secret-custody out of it.
	CloudMutate:  {Configured, Provisioned, Seeded, Converged, Operating, Destroyed},
	ClusterWrite: {Provisioned, Seeded, Converged, Operating, Destroyed},

	// FIRST ROW ADDED RATHER THAN WIDENED, because write-repo is the first grant
	// added since this table was written (see the Grant block in extension.go for
	// why it took four cases).
	//
	// The two states are the two moments `deliver-docs` runs, and it runs at both
	// by construction: copier invokes it from `_tasks`, which fire on render
	// (`llz new` → scaffolded) and on `copier update` (→ upgraded). Its own
	// repointInstanceRootLinks comment is about the second — the walk is gated on
	// template ownership precisely because on update it runs against a LIVE
	// instance holding files that are none of the template's business.
	//
	// NOTHING ELSE IS LISTED, and the omissions are deliberate rather than
	// pending. `promoted` looks obvious — promote-pipeline generates
	// .github/workflows/promote.yml — but that extension does not hold this grant:
	// its rendering is pure and its os.WriteFile stayed in package main, so adding
	// the state would list a row no shipping code exercises. The rule this table
	// has followed for both earlier widenings is that a state earns its place by an
	// extraction that needed it, not by seeming plausible. When promote-pipeline's
	// write moves in, `promoted` can be argued then, and there is a test that will
	// notice: TestGrantStatesIsPinned.
	//
	// Note this is also the first row whose states are all OUTSIDE the mutating
	// middle of the lifecycle. The other three rows start at `provisioned` — you
	// cannot mutate a cluster or a cloud before one exists. Repo writes are the
	// opposite shape: they happen when the REPO changes, which is before any
	// substrate exists and again when the template moves under it.
	WriteRepo: {Scaffolded, Upgraded},
}

// `Operating` was ADDED to CloudMutate by the fourth extension, and the row is
// worth reading as a worked example of the paragraph above.
//
// The original four states came from where the catalog places cloud-mutate — the
// `→ provisioned` and `→ destroyed` groups, where it is obvious. `assert-storage`
// then could not be declared: two of its three bindings are reconciler lanes wired
// into reconcile.go that run in-pod, continuously, and PUT tags and labels onto
// Linode Volumes. They SHIP. The model was refusing to describe code in production.
//
// Refusing it was not the conservative choice, it was wrong in the dangerous
// direction. A continuously-running cloud mutator is precisely what a reviewer most
// wants declared; a ceiling that makes it inexpressible does not prevent it, it
// only stops it being written down — which is `→ seeded` banned-by-omission again,
// in the half of the ceiling that exists to fix banning-by-omission.
//
// The catalog had the evidence and did not follow it: it flagged assert-storage as
// "holds cloud-mutate — the odd one out" and never carried that into this table.
// A flagged anomaly is a defect report, not a footnote.

func kinds() []BindingKind { return []BindingKind{Transition, Assertion, Invariant, Gate} }

// Validate reports every problem with an extension's declaration, not just the
// first — a caller fixing a manifest wants the whole list.
//
// The rules below ARE the ceiling. PR #15 expressed a ceiling as a closed menu of
// skeletons (`kind: check|tool`), which bans by omission: there was no `seeder`
// skeleton, so the 6,874 lines of credential provisioning the catalog found under
// `→ seeded` could not be expressed at all, and the ban was invisible until
// someone went looking. Here the dangerous thing is declarable and CHECKED
// against what the extension binds, so a refusal comes with a reason.
func (e Extension) Validate() []error {
	var errs []error

	if !validName(e.Name) {
		errs = append(errs, fmt.Errorf("name %q must be kebab-case (lowercase alphanumeric and single hyphens, not leading/trailing)", e.Name))
	}
	if strings.TrimSpace(e.Short) == "" {
		errs = append(errs, fmt.Errorf("%s: short description is required — it is what `llz extension list` shows", e.Name))
	}
	if len(e.Bindings) == 0 {
		errs = append(errs, fmt.Errorf("%s: needs at least one binding; an extension that attaches nowhere never runs", e.Name))
	}

	seenBinding := map[string]bool{}
	for _, b := range e.Bindings {
		allowed, ok := bindableStates[b.Kind]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: unknown binding kind %q (want %s)", e.Name, b.Kind, kindList()))
			continue
		}
		if !validState(b.State) {
			errs = append(errs, fmt.Errorf("%s: unknown state %q in binding %s", e.Name, b.State, b))
			continue
		}
		// A name is optional, but a malformed one is not: it is what distinguishes
		// repeated attachments in the duplicate key and in every message here.
		if b.Name != "" && !validName(b.Name) {
			errs = append(errs, fmt.Errorf("%s: binding name %q must be kebab-case (same rule as an extension name)", e.Name, b.Name))
		}
		if !containsState(allowed, b.State) {
			errs = append(errs, fmt.Errorf("%s: a %s binding cannot attach to %q (allowed: %s)",
				e.Name, b.Kind, b.State, stateList(allowed)))
		}
		// Keyed on kind:state:name, NOT on b.String() — that includes grants, so two
		// declarations of the same attachment carrying DIFFERENT grants would have
		// looked distinct. Same attachment twice is a mistake whatever it asks for,
		// and the conflicting-grants case is the one most worth catching.
		//
		// Name is part of the key so an extension can hold several invariants (see
		// Binding.Name); leaving it out capped every extension at one, since
		// `operating` is the only state an invariant may attach to. Unnamed
		// attachments still collide with each other, which is the common mistake.
		at := string(b.Kind) + ":" + string(b.State) + ":" + b.Name
		if seenBinding[at] {
			errs = append(errs, fmt.Errorf("%s: duplicate binding %s", e.Name, b))
		}
		seenBinding[at] = true
	}

	for i, note := range e.Incomplete {
		if strings.TrimSpace(note) == "" {
			errs = append(errs, fmt.Errorf("%s: Incomplete[%d] is blank — the field records WHAT is "+
				"not declared yet; an empty entry says an extension is partial without saying how", e.Name, i))
		}
	}

	for _, b := range e.Bindings {
		// A grant is the HANDLE, not a label on one: a cluster-read binding is
		// handed a read-only kubeconfig, a secret-custody binding an OpenBao token
		// fenced to its declared paths. So a binding that declares nothing is handed
		// nothing and cannot do anything — it is an incomplete declaration rather
		// than a modest one, and saying so here is cheaper than debugging an action
		// that receives an empty context.
		if len(b.Grants) == 0 {
			errs = append(errs, fmt.Errorf("%s: %s: needs at least one grant — a grant is the handle the action "+
				"receives, so a binding that asks for nothing is handed nothing", e.Name, b))
		}
		seenGrant := map[Grant]bool{}
		for _, g := range b.Grants {
			if !validGrant(g) {
				errs = append(errs, fmt.Errorf("%s: %s: unknown grant %q (want %s)", e.Name, b, g, grantList(Grants())))
				continue
			}
			if seenGrant[g] {
				errs = append(errs, fmt.Errorf("%s: %s: duplicate grant %q", e.Name, b, g))
			}
			seenGrant[g] = true
		}
		errs = append(errs, e.checkBindingCeiling(b)...)
	}

	return errs
}

// checkBindingCeiling enforces what ONE binding may touch, judged only on what
// that binding declares. Nothing another binding holds can widen or narrow it —
// that independence is the whole reason grants moved onto Binding.
func (e Extension) checkBindingCeiling(b Binding) []error {
	var errs []error

	switch b.Kind {
	case Gate:
		// A gate is file-in, findings-out. All six gates the catalog found need
		// nothing else, and a gate that reached a cluster or a credential would be
		// doing so in the pre-commit path against live infrastructure.
		for _, g := range b.Grants {
			if g != ReadRepo {
				errs = append(errs, fmt.Errorf("%s: %s: a gate permits only %q, not %q — "+
					"it runs in the fast pre-commit path over files alone", e.Name, b, ReadRepo, g))
			}
		}
	case Assertion:
		// An assertion observes; it does not change what it is measuring. The
		// catalog has exactly two entries that break this and flags both as
		// anomalies: assert-storage holds cloud-mutate ("the odd one out") and
		// wedge-gameday holds cluster-write ("so not a plain assertion"). Neither
		// is an exception worth carving out — the mutating half of each is a
		// TRANSITION that pairs with an assertion, which this model expresses as
		// two bindings on one extension, each with its own grants.
		for _, g := range b.Grants {
			if !readOnly[g] {
				errs = append(errs, fmt.Errorf("%s: %s: an assertion permits only read grants (%s), not %q — "+
					"if it must mutate, declare the mutating half as its own transition binding",
					e.Name, b, grantList(sortedReadOnly()), g))
			}
		}
	}

	// Where a mutating grant may be asked for. Applied to transition and invariant
	// only: gate and assertion already carry a blanket rule above, and running both
	// would report one mistake twice. These two kinds are exactly the pair that had
	// no ceiling at all, which is what grantStates exists to close.
	if b.Kind == Transition || b.Kind == Invariant {
		for _, g := range b.Grants {
			allowed, restricted := grantStates[g]
			if !restricted || containsState(allowed, b.State) {
				continue
			}
			errs = append(errs, fmt.Errorf("%s: %s: %q may only be asked for at %s — "+
				"a binding elsewhere that needs it is either mislabelled or is widening "+
				"what the state is understood to do", e.Name, b, g, stateList(allowed)))
		}
	}

	// Seeding is DEFINED by placing credential material. A binding that claims the
	// state without the grant has either mislabelled itself or is smuggling
	// custody past the reviewer who reads the grant line.
	if b.Kind == Transition && b.State == Seeded && !bindingHas(b, SecretCustody) {
		errs = append(errs, fmt.Errorf("%s: %s: a transition to %q must declare %q — "+
			"that transition is defined by placing credential material", e.Name, b, Seeded, SecretCustody))
	}

	// own-paths is the copier fence, and ADR 0014's corollary is that
	// .template-manifest is the ONE ownership authority — the grant is exactly the
	// manifest's `owned` class, whose meaning is "copier must not render these
	// bytes; something else does". That something else may be llz (`apl-values/*/**`
	// comes from `llz render`), another tool (`.terraform.lock.hcl` from `terraform
	// init`), or the operator (`kubernetes-custom/**`) — authorship is not what the
	// class turns on.
	//
	// WHICH IS WHY THE TWO STATES ARE RIGHT, and not merely a convention: a fence
	// is only meaningful when the thing it fences off runs, and copier runs at
	// exactly two moments — `llz new` (scaffolded) and `copier update` (upgraded).
	// A binding that WRITES a file at some other state does not thereby need the
	// grant; it needs its extension to have declared the fence once, at one of the
	// two moments copier could otherwise have clobbered it.
	//
	// The case that proves the distinction: promote-pipeline generates
	// `.github/workflows/promote.yml`, and the manifest classes that file `merge`,
	// not `owned` — it is a copier-rendered caller stub carrying `instance_repo`
	// plus a trigger surface an operator may tune. Generating a file is not
	// grounds for the grant; being outside copier's render is.
	if bindingHas(b, OwnPaths) && !(b.Kind == Transition && (b.State == Scaffolded || b.State == Upgraded)) {
		errs = append(errs, fmt.Errorf("%s: %s: %q is only meaningful on a transition to %q or %q — "+
			"it declares files the template must not re-render (ADR 0014)",
			e.Name, b, OwnPaths, Scaffolded, Upgraded))
	}

	return errs
}

func bindingHas(b Binding, g Grant) bool {
	for _, have := range b.Grants {
		if have == g {
			return true
		}
	}
	return false
}

// ValidateSet checks a whole registry: every extension individually, plus the
// cross-cutting rule that names are unique, since the name is how an operator
// enables, disables and refers to one.
//
// It has no production caller yet, and that is expected rather than an oversight:
// this whole package is the declaration model with the registry deliberately
// deferred (see the package doc). Uniqueness is genuinely a set-level rule — no
// single Extension can detect it — so it belongs here rather than being invented
// alongside the loader, and it is the entry point that loader will call.
func ValidateSet(exts []Extension) []error {
	var errs []error
	seen := map[string]int{}
	for _, e := range exts {
		errs = append(errs, e.Validate()...)
		seen[e.Name]++
	}
	var dupes []string
	for name, n := range seen {
		if n > 1 {
			dupes = append(dupes, fmt.Sprintf("%s (%d)", name, n))
		}
	}
	sort.Strings(dupes)
	for _, d := range dupes {
		errs = append(errs, fmt.Errorf("duplicate extension name %s — the name is how an operator enables and refers to one", d))
	}
	return errs
}

func containsState(states []State, s State) bool {
	for _, k := range states {
		if k == s {
			return true
		}
	}
	return false
}

func stateList(states []State) string {
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = string(s)
	}
	return strings.Join(out, ", ")
}

func kindList() string {
	out := make([]string, 0, 4)
	for _, k := range kinds() {
		out = append(out, string(k))
	}
	return strings.Join(out, ", ")
}

func sortedReadOnly() []Grant {
	var out []Grant
	for _, g := range Grants() {
		if readOnly[g] {
			out = append(out, g)
		}
	}
	return out
}
