package summarysecret

// extension.go — `guard-summary-secret` declares itself.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `guard-summary-secret` declaration.
//
//	gate:scaffolded[read-repo]
//
// A GATE, and one nothing else in the tree could have caught. `ghsecret.Mask`
// makes a value look handled — the `::add-mask::` line is right there in the
// diff — and every reviewer who saw the OpenBao init step read it as covering
// the fenced payload three lines below. It does not: masking redacts the LOG
// stream, and a job summary is a Markdown file GitHub renders untouched. No
// linter, no secret scanner and no test distinguishes the two channels, because
// syntactically they are the same `Append` call.
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
