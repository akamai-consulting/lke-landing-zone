package cosignguard

// extension.go — `guard-cosign-subject` declares itself.
//
// FOUND BY A SWEEP, NOT A HUNCH. This file measured CLOSURE 1 — `main`, and
// nothing else — and had done so for the whole campaign. It was found only after
// two "mechanical extraction is complete" calls were disproved by re-measuring,
// which is why the state file now opens every iteration with the per-file sweep.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `guard-cosign-subject` declaration.
//
//	gate:scaffolded[read-repo]
//
// A GATE, and a sharp one. Keyless signing derives the certificate subject from
// the signing workflow's PATH, so RENAMING A WORKFLOW silently invalidates every
// signature policy that pins it — verification keeps passing until the day an
// image is actually re-signed. Nothing else in the tree connects a workflow
// filename to a verification policy.
//
// `read-repo` and nothing else, which is what makes the gate kind legal:
// Validate() refuses a gate holding any other grant, and this one opens files and
// compares strings with no cluster and no cloud.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "guard-cosign-subject",
		Short:  "every workflow file named in a cosign keyless subject must still exist",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
