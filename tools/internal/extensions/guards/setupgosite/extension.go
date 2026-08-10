package setupgosite

// extension.go — `setup-go-sole-site` declares itself.
//
// A GATE IN THE PLAINEST SENSE: it reads Actions YAML and compares file paths.
// No cluster, no network, no clock — so `read-repo` and nothing else, which is
// what makes the gate kind legal for it.
//
// Its nearest sibling is `version-pins`, and the kinship is worth stating: both
// exist because a value with more than one home drifts, and neither can be caught
// by looking at any single site. version-pins holds restatements of a version
// EQUAL to an authority; this one holds the number of sites at ONE. The second is
// the stronger property and the cheaper check, and it is available here only
// because the composite made a single site possible in the first place.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `setup-go-sole-site` declaration.
//
//	gate:scaffolded[read-repo]
//
// `scaffolded`, not `built`: the property is decidable from the tree alone and
// the cheapest moment to say so is before the commit that carries the bypass —
// the same placement pin-coherence argues for, and for the same reason. Waiting
// until a release gate runs would mean discovering it on the tag.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "setup-go-sole-site",
		Short:  "the Go toolchain action may be referenced only by .github/actions/setup-llz",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
