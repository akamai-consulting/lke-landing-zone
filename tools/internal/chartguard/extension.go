package chartguard

// extension.go — `guard-charts` declares itself.
//
// TENTH EXTENSION, AND THE FOURTH GATE. It adds no new shape to the model, and
// that is why it was taken now: after nine extractions that each found something,
// the useful question is whether a routine one is routine yet. It was — one
// afternoon's shape, no ceiling probe needed, no vocabulary argument.
//
// The interesting part is what it dragged out with it. See internal/guardwalk.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `guard-charts` declaration.
//
// `gate:scaffolded[read-repo]`, the same shape `guard-budgets` and `guard-docs`
// take. Three checks, one binding, because they answer one question at one moment:
// is this commit's chart state publishable?
//
//   - chart-version — an edit inside a chart directory that does not move
//     Chart.yaml. `publish-charts` only ever pushes a NEW version, so an unmoved
//     pin 404s at Argo sync time.
//   - chart-lock — Chart.lock drifted from Chart.yaml's dependencies.
//   - chart-pin — a dependency pinned to a version the repo does not build.
//
// WHY ONE BINDING AND NOT THREE. `reconcile-actions` split into four named
// invariants because their GRANTS differ — the token restorers hold secret
// custody, the storage-class demoter does not. These three hold exactly the same
// grant and run at exactly the same moment, so naming them separately would buy
// nothing the checks' own names do not already say. The split is justified by
// divergent capability, not by count.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "guard-charts",
		Short:  "chart versions move, locks match, and pins resolve to something that exists",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
