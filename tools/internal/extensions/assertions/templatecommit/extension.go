package templatecommit

// extension.go — `template-commit` declares itself: resolve which template commit
// an instance is pinned to, and derive the CI image refs that follow from it.
//
// SEVENTY-SECOND EXTENSION. Closure 7 → 4 after two enablers, both of which were
// FACTS in the wrong file:
//
//	templateRepo   one line, six callers  -> templateid.Repo()
//	ciTofuTag,     two consts, three      -> versionpins.CITofuTag / CIKubernetesTag
//	ciKubernetesTag  callers
//
// THE SECOND ONE HAS A SCAR ATTACHED and it is why the move matters.
// `ciTofuTag` was still on 1.9.8 after build-images.yml and lint.yml had both
// moved — new instances would have been scaffolded onto a HashiCorp Terraform
// image while every caller invoked `tofu`. That is precisely the drift
// `internal/versionpins` exists to catch, so the value it catches drift IN should
// not live in a file about GitHub tokens.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `template-commit` declaration.
//
//	assertion:configured[read-repo, cloud-read]
//
// `cloud-read` is `ImagePublished`: before reporting an image ref as the one an
// instance should use, it asks the registry whether that image EXISTS. A pin to
// an unpublished image is the failure this prevents, and it is the reason this
// cannot be a gate however read-only it looks.
//
// `read-repo` for the answers file and the recorded pins it compares against.
//
// IT REPORTS SKEW; IT DOES NOT FIX IT. `StaleCIImageVars` returns what has
// drifted and the caller decides — `reportCIImageSkew` warns, `llz tokens`
// offers a repin. Keeping the computation here and the action in package main is
// what lets this declare no write grant at all, and it is the same split
// `internal/promote` refused to give up two moves ago.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "template-commit",
		Short:  "resolve the pinned template commit and report CI image refs that have drifted from it",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Assertion,
			State:  extension.Configured,
			Grants: []extension.Grant{extension.ReadRepo, extension.CloudRead},
		}},
		Incomplete: []string{
			"`assert-adopter-pin` is bound at `configured` and does not run against a " +
				"configured instance. It runs on the TEMPLATE REPO when a release " +
				"candidate is cut, and asserts that an adopter who scaffolds from that tag " +
				"WOULD reach `configured` coherently — tag resolves, `llz tokens` computes " +
				"an immutable image pin naming the same commit, both images are published, " +
				"and assert-image-fresh accepts a binary stamped there. Every grant and the " +
				"kind are exactly right; the STATE is a stand-in, because the model " +
				"describes an instance's lifecycle and has no word for a gate on the " +
				"template's own release. That is a different gap from `pin-coherence`'s " +
				"(a check that runs AFTER a state to catch what it broke) and from " +
				"`argocd-diagnostics`' (a diagnostic that runs when a state FAILED), so it " +
				"is recorded as its own rather than folded into either.",
		},
	}
}

// pinBinding returns the assertion whose grants scope this package's forge access.
// By kind, not by index — the reason obj-encryption's seedBinding records.
func pinBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Kind == extension.Assertion {
			return b
		}
	}
	panic("template-commit: no assertion binding — the forge handle is built from it")
}
