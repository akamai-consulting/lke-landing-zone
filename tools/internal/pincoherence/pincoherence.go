package pincoherence

// Package pincoherence guards the ONE remaining place an instance records its
// template version twice: .copier-answers.yml carries both `_commit` (copier's
// own record of the template state merged into this tree) and `llz_version`
// (the answer `llz upgrade` passes with --data).
//
// They are the same fact, and nothing made them agree. Worse, the readers PREFER
// the one copier does not write: pinnedTemplateRef and resolveTemplateRef both
// take llz_version first and fall back to _commit. So a `copier update` that did
// not actually apply leaves llz_version at the new tag while _commit still names
// the old one — and every consumer resolves the NEW tag against a tree that only
// ever received the OLD scaffold.
//
// That is not hypothetical and it is not cosmetic. A live instance sat at
// `_commit: v0.0.33` / `llz_version: v0.0.34`, so ArgoCD fetched the shared
// platform-apl manifests (RemoteBase, pinned from llz_version) at v0.0.34 while
// the instance's own vendored workflows and TF roots were v0.0.33. The skew is a
// difference in what is DEPLOYED, and it is silent: both values are individually
// well-formed, so no parse, render, or lint step had any reason to object.
//
// The guard is deliberately narrow — it fires only when BOTH pins are exact
// release tags and they differ. A guard that false-positives on the legitimate
// non-tag forms (see exactReleaseTag) is one an operator learns to skip past,
// which is worth less than the narrow one it replaces.

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/answers"
)

// exactReleaseTag matches ONLY a bare release tag (v1.2.3). It is deliberately
// stricter than releaseSemver (render.go), which also accepts a pre-release/build
// suffix and therefore matches copier's `git describe` form (v1.2.3-5-gabc1234).
// Comparing a describe string against a raw SHA says nothing, so the two pins are
// only held to agree when each is an exact tag.
var exactReleaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// Assert fails when dir's .copier-answers.yml names two different
// release tags for the one template pin. Silent (nil) when there is no instance
// there, when either pin is absent, or when either is not an exact release tag.
func Assert(dir string) error {
	a, err := answers.Read(dir)
	if err != nil || a == nil {
		return nil //nolint:nilerr // no readable answers file — not an instance; other steps report that
	}
	commit, version := strings.TrimSpace(a.Commit), strings.TrimSpace(a.Version)
	if !exactReleaseTag.MatchString(commit) || !exactReleaseTag.MatchString(version) || commit == version {
		return nil
	}
	//lint:ignore ST1005 multi-line operator diagnostic: the period precedes an embedded newline explaining what the two pins record
	return fmt.Errorf("template pin skew in .copier-answers.yml: `_commit: %s` but `llz_version: %s`.\n"+
		"  These record the same fact. Everything that resolves the pin (pinnedTemplateRef, resolveTemplateRef —\n"+
		"  so the vendored workflows' TF_IMAGE check AND the apl-values remote refs ArgoCD syncs) prefers\n"+
		"  llz_version, while copier's own record says this tree only ever received %s. The usual cause is a\n"+
		"  `copier update` that did not apply — leaving the instance DEPLOYING %s manifests from a %s scaffold.\n"+
		"  Fix: re-run `llz upgrade --ref %s` so copier rewrites both, then `llz render` and commit the result.",
		commit, version, commit, version, commit, version)
}
