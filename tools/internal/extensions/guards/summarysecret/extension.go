package summarysecret

// extension.go — `guard-summary-secret` declares itself.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `guard-summary-secret` declaration.
//
//	gate:scaffolded[read-repo]
//
// A GATE, and one nothing else in the tree could have caught: no linter, secret
// scanner or test distinguishes a log write from a job-summary write, because
// syntactically they are the same `Append` call. guard.go carries the scar.
//
// `read-repo` and nothing else, which is what makes the gate kind legal:
// Validate() refuses a gate holding any other grant, and this one parses Go
// source and compares strings with no cluster and no cloud.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "guard-summary-secret",
		Short:  "a function that masks secrets may not write computed values to the job summary",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
