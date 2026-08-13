package runinjection

// extension.go — `workflow-injection` declares itself.
//
// A gate in the plainest sense: it reads Actions YAML and compares expressions
// against a rule. No cluster, no network, no clock — so `read-repo` and nothing
// else.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `workflow-injection` declaration.
//
//	gate:scaffolded[read-repo]
//
// `scaffolded`, not a live assertion, because the property is decidable from the
// tree — and it has to be caught there. At runtime a successful injection looks
// exactly like a successful step: the workflow does what the attacker asked and
// reports green. There is nothing downstream for an assertion to observe.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "workflow-injection",
		Short:  "no externally-supplied expression may be interpolated into a run: script",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
