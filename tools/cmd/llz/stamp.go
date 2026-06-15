package main

// stamp.go ports template-scripts/stamp-template-version.sh into llz so the
// operator path (`llz env add`, `llz upgrade`) can record `.template-version`
// inside a rendered instance, which carries no scripts/ tree. The bash version
// still ships for template-repo CI (release-e2e) that runs from a checkout.
//
// `.template-version` is the provenance `llz drift` (and the Scheduled Checks
// template-drift job, which runs it) reads to report how far behind the template
// an instance has fallen. In an instance the best provenance is
// .copier-answers.yml (_src_path + _commit, written by copier); we fall back to
// git remotes/HEAD when those are absent (a template-repo checkout).

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const defaultTemplateRepo = "akamai-consulting/lke-landing-zone"

type templateVersion struct {
	Schema       int    `json:"schema"`
	TemplateRepo string `json:"template_repo"`
	TemplateRef  string `json:"template_ref"`
	TemplateSHA  string `json:"template_sha"`
	Generator    string `json:"generator"`
	StampedAt    string `json:"stamped_at"`
	Env          string `json:"env"`
}

// stampTemplateVersion writes .template-version at the repo root, recording which
// template repo/ref/commit this instance was generated from. env is recorded
// informationally; if empty, the env from an existing stamp is preserved.
func stampTemplateVersion(env string) error {
	tv := templateVersion{Schema: 1, Generator: "llz", Env: env}

	if a, _ := readAnswers("."); a != nil {
		tv.TemplateRepo = normalizeTemplateRepo(a.SrcPath)
		tv.TemplateSHA = a.Commit
		tv.TemplateRef = a.Commit
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
	// Preserve the first-seen env when none was passed.
	if tv.Env == "" {
		if b, err := os.ReadFile(".template-version"); err == nil {
			var prev templateVersion
			if json.Unmarshal(b, &prev) == nil {
				tv.Env = prev.Env
			}
		}
	}
	tv.StampedAt = time.Now().UTC().Format("2006-01-02T15:04:05Z")

	b, err := json.MarshalIndent(tv, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(".template-version", append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("Stamped .template-version: %s @ %s (%.8s)\n", tv.TemplateRepo, tv.TemplateRef, tv.TemplateSHA)
	return nil
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
