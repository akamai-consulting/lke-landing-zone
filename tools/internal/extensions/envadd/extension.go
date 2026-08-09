package envadd

// extension.go — `env-add` declares itself: add a deployment environment to an
// instance that already exists.
//
// SEVENTY-SEVENTH MOVE. It was `scaffold.go`, and it spent four iterations
// recorded here as BLOCKED — twice with an argument for why the block was
// correct. Both blockers dissolved without it being touched: `runRender` became
// internal/render (move 76), and `envAddOpts` became internal/envdef (move 69).
// What was left was one edge.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `env-add` declaration.
//
//	transition:configured[read-repo]
//
// `configured` because that is what adding an environment DOES — it is the moment
// an instance gains a deployment target. The heavy lifting is delegated:
// internal/envdef writes the YAML, internal/render turns it into tfvars, and
// internal/instanceresolve answered whether the region and object-storage cluster
// were real before any of it ran.
//
// SO THIS PACKAGE ORCHESTRATES AND REPORTS. `groupFindings` and
// `printPlaceholderChecklist` are most of what is left: after the files are
// written it tells the operator which placeholders still need a human. That is
// why `read-repo` is the only grant — the writes belong to the packages that own
// them, and this one reads the result back to describe it.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "env-add",
		Short:  "add a deployment environment to an existing instance and report what still needs a human",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			State:  extension.Configured,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
