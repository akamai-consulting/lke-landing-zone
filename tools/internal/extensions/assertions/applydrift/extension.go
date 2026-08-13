package applydrift

// extension.go — `apply-drift` declares itself.
//
// It reads the forge's run history and the local git log, and changes nothing.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `apply-drift` declaration.
//
//	assertion:provisioned[read-repo]
//
// ASSERTION, NOT GATE, and the tree enforces the distinction by directory: a gate
// is decidable from the repository contents alone, and this is not. It reads the
// FORGE — the workflow-run history — to learn what was last applied. No amount of
// reading this checkout answers that.
//
// `provisioned`, not `scaffolded`: the question only has meaning once a
// deployment has been applied at least once. Before that there is no last-apply
// commit to diff against, and the verb says so rather than reporting a fresh
// instance as infinitely behind.
//
// `read-repo` is the grant, and it is the same approximation `promote-pipeline`
// and `upstream-updates` carry: this also reads the FORGE, for which the
// vocabulary still has no term. Recorded rather than invented — see
// seambypass_test.go, which measures what such a capability would have to cover.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "apply-drift",
		Short:  "report Terraform or spec changes merged since a deployment's last successful apply",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Assertion,
			State:  extension.Provisioned,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
