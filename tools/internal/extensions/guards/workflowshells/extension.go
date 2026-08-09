package workflowshells

// extension.go — `guard-workflow-shells` declares itself.
//
// FOUND BY A SWEEP, NOT A HUNCH. This file measured CLOSURE 1 — `main`, and
// nothing else — and had done so for the whole campaign. It was found only after
// two "mechanical extraction is complete" calls were disproved by re-measuring,
// which is why the state file now opens every iteration with the per-file sweep.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `guard-workflow-shells` declaration.
//
//	gate:scaffolded[read-repo]
//
// A GATE over workflow YAML. GitHub uses bash for `run:` steps on the host but
// falls back to `sh` (dash) inside a `container:`, where `set -o pipefail` is a
// syntax error — so a step that looks hardened is silently not. Same shape as the
// other guards here: the thing that fails is not the thing that reports.
//
// `read-repo` and nothing else, which is what makes the gate kind legal:
// Validate() refuses a gate holding any other grant, and this one opens files and
// compares strings with no cluster and no cloud.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "guard-workflow-shells",
		Short:  "a run: step inside a container: must declare bash, not fall back to sh",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}

// workflowShellsBinding is the gate binding this guard reads through, looked up
// rather than reconstructed so a second binding cannot silently widen it.
func workflowShellsBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Kind == extension.Gate {
			return b
		}
	}
	panic("guard-workflow-shells: no gate binding — the scan builds its read-repo " +
		"reader from it, so its absence is a wiring bug")
}
