package coverageguard

// extension.go — `guard-coverage` declares itself.
//
// FOUND BY A SWEEP, NOT A HUNCH. This file measured CLOSURE 1 — `main`, and
// nothing else — and had done so for the whole campaign. It was found only after
// two "mechanical extraction is complete" calls were disproved by re-measuring,
// which is why the state file now opens every iteration with the per-file sweep.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `guard-coverage` declaration.
//
//	gate:scaffolded[read-repo]
//
// A GATE over FILES ALONE: a coverprofile and a list of `<pkg-suffix>=<min>` floors.
//
// It is the gate this whole campaign has been running against — every extraction
// in the ledger had to clear it, and every lowered floor is recorded in
// .core-surface-budget.yaml because of it. Extracting it puts the thing that
// judges coverage under a coverage floor of its own, which is the same closing of
// the loop `assert-suite` got when the runner of the assertions became one.
//
// `read-repo` and nothing else, which is what makes the gate kind legal:
// Validate() refuses a gate holding any other grant, and this one opens files and
// compares strings with no cluster and no cloud.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "guard-coverage",
		Short:  "enforce per-package minimum statement coverage from a Go coverprofile",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}, {
			// THE `--bank` HALF NEEDS ITS OWN BINDING, and the model is what said
			// so: a Gate may hold read-repo and nothing else, so the floors cannot
			// be raised through the binding that reads the profile. Same shape and
			// same reason as template-sustain's `lock-refresh` — a `--write` that
			// borrowed its gate's grant would be a gate that writes, which is the
			// one thing the kind exists to forbid.
			//
			// A TRANSITION rather than a second gate, because it MUTATES: it edits
			// COVERAGE_MINS in the Makefile to match what the packages measure.
			// Tighten-only, and it refuses to run while anything is red — see
			// bank.go for why both rules are load-bearing.
			Kind:   extension.Transition,
			Name:   "floor-bank",
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo, extension.WriteRepo},
		}},
	}
}

// bankBinding is the binding `--bank` writes through. Looked up by NAME rather
// than by kind: the extension has two bindings now, and picking "the one that is
// not a gate" would silently start returning something else the moment a third
// appears.
func bankBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Kind == extension.Transition && b.Name == "floor-bank" {
			return b
		}
	}
	panic("guard-coverage: no floor-bank binding — `--bank` builds its writer from " +
		"one, so its absence is a wiring bug")
}

// coverageBinding is the gate binding this guard reads its coverprofile through.
func coverageBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Kind == extension.Gate {
			return b
		}
	}
	panic("guard-coverage: no gate binding — reading the coverprofile builds its " +
		"read-repo reader from it, so its absence is a wiring bug")
}
