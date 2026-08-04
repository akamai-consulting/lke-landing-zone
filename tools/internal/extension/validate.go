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
// Assertion may target any spine state, not just verified. That is a finding, not
// a generalisation for its own sake: the catalog identified config-readiness as
// "the configured predicate" and — the single most valuable split it found —
// ci_health.go as an action (converge) fused with a predicate (health). Under
// this model those separate, and health becomes a `converged` assertion. If
// assertions could only target `verified`, that split would have nowhere to land.
var bindableStates = map[BindingKind][]State{
	Transition: {Scaffolded, Configured, Provisioned, Seeded, Converged, Promoted, Upgraded, Destroyed},
	Assertion:  {Scaffolded, Configured, Provisioned, Seeded, Converged, Verified, Operating},
	Invariant:  {Operating},
	Gate:       {Scaffolded, Configured},
}

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
		if !containsState(allowed, b.State) {
			errs = append(errs, fmt.Errorf("%s: a %s binding cannot attach to %q (allowed: %s)",
				e.Name, b.Kind, b.State, stateList(allowed)))
		}
		// Keyed on kind:state, NOT on b.String() — that includes grants now, so two
		// declarations of the same attachment carrying DIFFERENT grants would have
		// looked distinct. Same attachment twice is a mistake whatever it asks for,
		// and the conflicting-grants case is the one most worth catching.
		at := string(b.Kind) + ":" + string(b.State)
		if seenBinding[at] {
			errs = append(errs, fmt.Errorf("%s: duplicate binding %s", e.Name, at))
		}
		seenBinding[at] = true
	}

	for _, b := range e.Bindings {
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

	// Seeding is DEFINED by placing credential material. A binding that claims the
	// state without the grant has either mislabelled itself or is smuggling
	// custody past the reviewer who reads the grant line.
	if b.Kind == Transition && b.State == Seeded && !bindingHas(b, SecretCustody) {
		errs = append(errs, fmt.Errorf("%s: %s: a transition to %q must declare %q — "+
			"that transition is defined by placing credential material", e.Name, b, Seeded, SecretCustody))
	}

	// own-paths is the copier fence, and ADR 0014's corollary is that
	// .template-manifest is the ONE ownership authority. It is only meaningful
	// where files are written or re-rendered.
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
