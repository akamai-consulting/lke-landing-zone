package upgrade

// extension.go — `upgrade` declares itself: re-render an instance from a newer
// template release, and tell the operator what it could not do for them.
//
// SEVENTY-SEVENTH EXTENSION, AND THE FIRST WHOSE STATE IS ITS OWN NAME. Every
// binding so far has attached to a state some OTHER command establishes. This one
// IS `upgraded` — the state exists because this command is what reaches it — and
// that made the declaration the easiest in the campaign rather than the hardest:
// `write-repo` carries {scaffolded, upgraded}, and this is the `upgraded` half
// that the row was shaped around when `deliver-docs` added it.
//
// THE ANSWER-MAP HELPERS CAME WITH IT. answerRegressions, currentAnswerMap and
// readAnswerMap lived in ci_upgrade_test_gate.go — a file about a CI gate — and
// two of them had no caller outside this command. They are exported now only
// because that gate genuinely still calls them; currentAnswerMap does not, and
// stayed unexported, which is the whole test of whether an export is a seam.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `upgrade` declaration.
//
//	transition:upgraded[read-repo, write-repo, own-paths]
//
// `own-paths` is the grant that is easy to miss and is the point of the command.
// `copier update` re-renders every templated file, and the only thing standing
// between an operator's edits and a silent overwrite is the manifest's ownership
// classes — which is why sustain.ApplyTemplateRemovals runs here and why the
// conflict scan exists. An upgrade that did not hold this grant would be a
// restore.
//
// WHAT IT DELIBERATELY DOES NOT HOLD is `cloud-mutate` or `secret-custody`, and
// reportCIImageSkew is where that shows. The upgrade moves the template pin, which
// makes the instance's TF_IMAGE/KUBE_IMAGE repo variables stale — and the command
// WARNS rather than fixes, because the authoritative copy of those values is a
// GitHub repo variable and pushing it would make an otherwise-local command need
// credentials. The warning naming `llz tokens` and `gh variable set` is the whole
// remediation surface.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "upgrade",
		Short:  "re-render this instance from a newer template release",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			State:  extension.Upgraded,
			Grants: []extension.Grant{extension.ReadRepo, extension.WriteRepo, extension.OwnPaths},
		}},
	}
}
