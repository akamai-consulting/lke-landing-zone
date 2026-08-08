package sustain

// stamp.go resolves an instance's template PROVENANCE — which template repo/ref/
// commit it was generated from — for `llz drift` (and the Scheduled Checks
// template-drift job that runs it).
//
// This used to write a committed `.template-version` file. It no longer does.
// The provenance was already recorded by copier in `.copier-answers.yml`
// (_src_path + _commit), so the stamp was a second copy of a fact llz did not
// own, and it churned on every upgrade (template_ref + template_sha + a
// stamped_at that moved even when nothing else did). Worse, the two could
// disagree: on an upgrade aborted by merge-conflict markers, llz rolled ITS
// stamp back to the old ref while copier's answers file — the one copier
// actually reads to compute the next update — stayed at the new one.
//
// So provenance is now DERIVED, never stored: .copier-answers.yml first, then
// git remotes/HEAD (a template-repo checkout), then the legacy .template-version
// of an instance that has not upgraded past it yet.

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/templateid"
)

// TemplateVersion is the resolved provenance `llz drift` reports on. It keeps the
// legacy .template-version JSON field names so a not-yet-upgraded instance's
// stamp still unmarshals into it.
type TemplateVersion struct {
	Schema       int    `json:"schema"`
	TemplateRepo string `json:"template_repo"`
	TemplateRef  string `json:"template_ref"`
	TemplateSHA  string `json:"template_sha"`
	Generator    string `json:"generator"`
	StampedAt    string `json:"stamped_at"`
	Env          string `json:"env"`
}

// ResolveTemplateVersion derives where this checkout came from. Pure resolution —
// it writes nothing. Order: copier's answers (the authority for an instance), then
// git remotes/HEAD (a template-repo checkout), then the legacy stamp.
func ResolveTemplateVersion(d Deps) TemplateVersion {
	tv := TemplateVersion{Schema: 1, Generator: "llz"}

	if a, _ := d.ReadAnswers("."); a != nil {
		tv.TemplateRepo = templateid.NormalizeTemplateRepo(a.SrcPath)
		tv.TemplateSHA = a.Commit
		tv.TemplateRef = firstNonEmpty(a.Version, a.Commit)
	}
	// A legacy instance still carrying the retired stamp: use it to fill any gap,
	// so `llz drift` keeps working there right up until `llz upgrade` deletes it.
	if tv.TemplateRepo == "" || tv.TemplateSHA == "" || tv.TemplateRef == "" {
		if b, err := os.ReadFile(".template-version"); err == nil {
			var prev TemplateVersion
			if json.Unmarshal(b, &prev) == nil {
				tv.TemplateRepo = firstNonEmpty(tv.TemplateRepo, prev.TemplateRepo)
				tv.TemplateSHA = firstNonEmpty(tv.TemplateSHA, prev.TemplateSHA)
				tv.TemplateRef = firstNonEmpty(tv.TemplateRef, prev.TemplateRef)
				tv.StampedAt = prev.StampedAt
				tv.Env = prev.Env
			}
		}
	}
	if tv.TemplateRepo == "" {
		tv.TemplateRepo = templateid.NormalizeTemplateRepo(gitOut(d, "remote", "get-url", "upstream"))
	}
	if tv.TemplateRepo == "" {
		tv.TemplateRepo = templateid.NormalizeTemplateRepo(gitOut(d, "remote", "get-url", "origin"))
	}
	if tv.TemplateRepo == "" {
		tv.TemplateRepo = templateid.DefaultRepo
	}
	if tv.TemplateSHA == "" {
		tv.TemplateSHA = gitOut(d, "rev-parse", "HEAD")
	}
	if tv.TemplateRef == "" {
		if ref := gitOut(d, "describe", "--tags", "--always"); ref != "" {
			tv.TemplateRef = ref
		} else {
			tv.TemplateRef = gitOut(d, "rev-parse", "--abbrev-ref", "HEAD")
		}
	}
	return tv
}

// gitOut runs a git command and returns trimmed stdout, or "" on any error.
func gitOut(d Deps, args ...string) string {
	out, err := d.Exec("git", args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// firstNonEmpty returns the first non-empty string. Local glue, copied rather than
// shared: three lines with nothing to drift.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
