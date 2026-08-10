package callerperms

// extension.go — `reusable-workflow-caller-permissions` declares itself.
//
// A gate in the plainest sense: it reads Actions YAML and compares permission
// sets. No cluster, no network, no clock — so `read-repo` and nothing else.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `reusable-workflow-caller-permissions` declaration.
//
//	gate:scaffolded[read-repo]
//
// `scaffolded`, not a live assertion, because the property is decidable from the
// tree — and it HAS to be caught there. The runtime signal is `startup_failure`:
// no jobs, no logs, no annotations. There is nothing downstream for an assertion
// to observe, which is exactly why this class survived in a delivered workflow
// across every scheduled run.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "reusable-workflow-caller-permissions",
		Short:  "a job calling a local reusable workflow must grant the union its jobs ask for",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
