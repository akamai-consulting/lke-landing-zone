package dependabotcoverage

// extension.go — `dependabot-coverage` declares itself.
//
// A GATE IN THE PLAINEST SENSE: it walks the tree for dependency manifests and
// compares what it finds against .github/dependabot.yml. No cluster, no network,
// no clock — `read-repo` and nothing else, which is what makes the gate kind
// legal for it.
//
// SCAFFOLDED, NOT BUILT, and the placement is the point. A manifest that nothing
// scans produces no error at any later moment: the build is green, the release is
// green, and the only symptom is a pin that stops moving. The PR that adds the
// manifest — or moves an existing one into a new directory — is the only place
// the omission is still cheap to see.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `dependabot-coverage` declaration.
//
//	gate:scaffolded[read-repo]
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "dependabot-coverage",
		Short:  "every dependency manifest is scanned by dependabot.yml or excluded with a reason",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
