package monitoringlabel

// extension.go — `guard-monitoring-labels` declares itself.
//
// FOUND BY A SWEEP, NOT A HUNCH. This file measured CLOSURE 1 — `main`, and
// nothing else — and had done so for the whole campaign. It was found only after
// two "mechanical extraction is complete" calls were disproved by re-measuring,
// which is why the state file now opens every iteration with the per-file sweep.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `guard-monitoring-labels` declaration.
//
//	gate:scaffolded[read-repo]
//
// A GATE extracted from the day-2-observability-blind outage (#175). apl-core v6's
// Prometheus selects monitoring CRs by {prometheus: system} across its
// serviceMonitor, rule AND podMonitor selectors. A CR without that label is not an
// error anywhere — it is simply never scraped, which is the failure mode this
// campaign keeps finding: a non-answer that reads as an absence.
//
// `read-repo` and nothing else, which is what makes the gate kind legal:
// Validate() refuses a gate holding any other grant, and this one opens files and
// compares strings with no cluster and no cloud.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "guard-monitoring-labels",
		Short:  "monitoring CRs must carry the label apl-core Prometheus selects on",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}

// gateBinding is the binding this guard reads through, looked up rather than
// reconstructed so that a second binding cannot silently widen what it may read.
func gateBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Kind == extension.Gate {
			return b
		}
	}
	panic("guard-monitoring-labels: no gate binding — the scan builds its read-repo " +
		"reader from it, so its absence is a wiring bug")
}
