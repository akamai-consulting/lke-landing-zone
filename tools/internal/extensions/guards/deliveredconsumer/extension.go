package deliveredconsumer

// extension.go — `delivered-consumer-guard` declares itself.
//
// A GATE IN THE PLAINEST SENSE: it reads .template-manifest and the Go sources
// under tools/, and compares two lists. No cluster, no network, no clock — so
// `read-repo` and nothing else, which is what makes the gate kind legal for it.
//
// Its nearest sibling is `template-manifest-check`, and the difference is the
// point: that one asserts every scaffold file IS classified, so the upgrade
// tooling never has to guess. This one asserts every file classified `managed`
// still has something that READS it. A file can be perfectly classified and
// perfectly dead, which is the state apl-values/values.yaml was in for a year.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `delivered-consumer-guard` declaration.
//
//	gate:scaffolded[read-repo]
//
// `scaffolded`, not `built`: the property is decidable from the tree alone, and
// the cheapest moment to say so is the commit that retires a consumer — not a
// release, and certainly not an adopter's incident.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "delivered-consumer-guard",
		Short:  "every `managed` file an instance carries names a consumer that still exists",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
