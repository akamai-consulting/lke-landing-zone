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
		}},
	}
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
