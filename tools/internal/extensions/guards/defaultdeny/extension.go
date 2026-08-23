package defaultdeny

// extension.go — `default-deny-egress` declares itself.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `default-deny-egress` declaration.
//
//	gate:scaffolded[read-repo]
//
// A GATE, and one earned the way mesh-egress was. A pod with no egress allow
// under a namespace default-deny is 1/1 Running with healthy endpoints and
// reaches nothing — not DNS, not the apiserver. There is no status, no event and
// no restart to observe, so a live assertion would have to know what the pod was
// supposed to reach before it could tell. The tree knows already.
//
// `read-repo` and nothing else, which is what makes the gate kind legal: it opens
// files and compares label maps, with no cluster and no cloud.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "default-deny-egress",
		Short:  "a pod whose egress is policed must be granted some egress",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
