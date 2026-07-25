package main

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
)

const defaultTemplateRepo = "akamai-consulting/lke-landing-zone"

// templateVersion is the resolved provenance `llz drift` reports on. It keeps the
// legacy .template-version JSON field names so a not-yet-upgraded instance's
// stamp still unmarshals into it.
type templateVersion struct {
	Schema       int    `json:"schema"`
	TemplateRepo string `json:"template_repo"`
	TemplateRef  string `json:"template_ref"`
	TemplateSHA  string `json:"template_sha"`
	Generator    string `json:"generator"`
	StampedAt    string `json:"stamped_at"`
	Env          string `json:"env"`
}

// resolveTemplateVersion derives where this checkout came from. Pure resolution —
// it writes nothing. Order: copier's answers (the authority for an instance), then
// git remotes/HEAD (a template-repo checkout), then the legacy stamp.
func resolveTemplateVersion() templateVersion {
	tv := templateVersion{Schema: 1, Generator: "llz"}

	if a, _ := readAnswers("."); a != nil {
		tv.TemplateRepo = normalizeTemplateRepo(a.SrcPath)
		tv.TemplateSHA = a.Commit
		tv.TemplateRef = firstNonEmpty(a.Version, a.Commit)
	}
	// A legacy instance still carrying the retired stamp: use it to fill any gap,
	// so `llz drift` keeps working there right up until `llz upgrade` deletes it.
	if tv.TemplateRepo == "" || tv.TemplateSHA == "" || tv.TemplateRef == "" {
		if b, err := os.ReadFile(".template-version"); err == nil {
			var prev templateVersion
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
		tv.TemplateRepo = normalizeTemplateRepo(gitOut("remote", "get-url", "upstream"))
	}
	if tv.TemplateRepo == "" {
		tv.TemplateRepo = normalizeTemplateRepo(gitOut("remote", "get-url", "origin"))
	}
	if tv.TemplateRepo == "" {
		tv.TemplateRepo = defaultTemplateRepo
	}
	if tv.TemplateSHA == "" {
		tv.TemplateSHA = gitOut("rev-parse", "HEAD")
	}
	if tv.TemplateRef == "" {
		if ref := gitOut("describe", "--tags", "--always"); ref != "" {
			tv.TemplateRef = ref
		} else {
			tv.TemplateRef = gitOut("rev-parse", "--abbrev-ref", "HEAD")
		}
	}
	return tv
}

// normalizeTemplateRepo turns a copier _src_path / git remote into an owner/repo
// slug when it is a github reference; otherwise returns it unchanged (a non-github
// host stays a full URL, which `llz drift` handles).
func normalizeTemplateRepo(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(s, "gh:"):
		return strings.TrimSuffix(strings.TrimPrefix(s, "gh:"), ".git")
	case strings.HasPrefix(s, "git@github.com:"):
		return strings.TrimSuffix(strings.TrimPrefix(s, "git@github.com:"), ".git")
	case strings.Contains(s, "github.com/"):
		return strings.TrimSuffix(s[strings.Index(s, "github.com/")+len("github.com/"):], ".git")
	default:
		return s // some other host / local path — leave as-is
	}
}

// gitOut runs a git command and returns trimmed stdout, or "" on any error.
func gitOut(args ...string) string {
	out, err := execOutput("git", args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
