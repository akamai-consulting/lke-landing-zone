package registry

// enablement.go — which extensions an INSTANCE has, as against which this binary
// was built with.
//
// The model's headline promise is that `Always` is a DEFAULT, not a constant: an
// instance with no object storage must be able to turn `assert-objstore` off in
// its own configuration rather than by taking a different build. Until now nothing
// read the field except the `llz extension list` display, so the promise was
// undeliverable and nothing said so.
//
// IT REUSES THE SPEC'S EXISTING ENABLEMENT RATHER THAN ADDING A SECOND ONE.
// `spec.components` already carries a tri-state toggle per component, with
// Mandatory, DependsOn, and defaults filled by Defaults(). An operator already
// turns object storage off there. Adding an `extensions:` map beside it would have
// meant two places to say one thing — and the failure mode of two places is the one
// this repo has been burned by most (a registry transcribing what the code says,
// with only a test to catch the drift).
//
// So an extension names the component it FOLLOWS, and enablement is derived. The
// connection already existed in prose: obj-encryption's declaration said "once,
// when spec.components.objProxy is enabled" and nothing but that comment linked
// them.
//
// WHAT IT DELIBERATELY DOES NOT DO. It does not disable anything. Nothing
// dispatches on the result yet beyond `llz ci gates` and the list display, and a
// resolver that silently stopped running an assert lane would be a behaviour change
// hiding inside a refactor. This answers "which extensions does this instance
// have?"; making the answer load-bearing everywhere is the next slice, and the
// gates driver is where it starts because a gate can be skipped harmlessly.

import (
	"fmt"
	"sort"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// Enablement is one extension's resolved state, with the reason attached. The
// reason is not decoration: an operator asking why a lane did not run needs to
// distinguish "you turned the component off" from "this was never on by default".
type Enablement struct {
	Extension extension.Extension
	Enabled   bool
	// Reason is a short phrase for `llz extension list`, e.g. "component objProxy
	// disabled" or "always".
	Reason string
}

// EnabledFor resolves every registered extension against an instance's component
// toggles, sorted by name.
//
// THREE RULES, IN ORDER, and the order is the whole content:
//
//  1. An extension naming a COMPONENT follows that component, in both directions.
//     This is what lets an instance turn a capability off — and equally, what lets
//     one turn an `Always: false` capability ON by enabling its component, which
//     the model's own note demands ("the registry must let an instance override
//     this field in both directions").
//  2. Otherwise `Always` decides.
//  3. A component name that matches nothing is an ERROR, not a default. Silently
//     treating a typo as "no component" would resolve to rule 2 and enable an
//     extension the operator believes they disabled — a failure in the dangerous
//     direction, and invisible.
func EnabledFor(toggles map[string]clusterspec.ComponentToggle) ([]Enablement, error) {
	var out []Enablement
	var unknown []string

	for _, e := range All() {
		switch {
		case e.Component == "":
			reason := "opt-in default"
			if e.Always {
				reason = "always"
			}
			out = append(out, Enablement{Extension: e, Enabled: e.Always, Reason: reason})
		default:
			if _, ok := clusterspec.LookupComponent(e.Component); !ok {
				unknown = append(unknown, fmt.Sprintf("%s names component %q", e.Name, e.Component))
				continue
			}
			on := clusterspec.ComponentEnabled(toggles, e.Component)
			reason := "component " + e.Component
			if !on {
				reason += " disabled"
			}
			out = append(out, Enablement{Extension: e, Enabled: on, Reason: reason})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Extension.Name < out[j].Extension.Name })

	if len(unknown) > 0 {
		sort.Strings(unknown)
		return out, fmt.Errorf("extension(s) name a component that does not exist: %v — a "+
			"misspelled component would resolve to `Always` and enable something the operator "+
			"believes they turned off, so it is refused rather than defaulted", unknown)
	}
	return out, nil
}

// ComponentLinks returns each extension that follows a component, for the tests
// that pin the mapping and for anything that needs to explain it.
func ComponentLinks() map[string]string {
	out := map[string]string{}
	for _, e := range All() {
		if e.Component != "" {
			out[e.Name] = e.Component
		}
	}
	return out
}
