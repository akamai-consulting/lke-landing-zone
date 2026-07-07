package main

// stamp.go ports template-scripts/stamp-template-version.sh into llz so the
// operator path (`llz env add`, `llz upgrade`) and template-repo CI can record
// `.template-version` without carrying a scripts/ tree.
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

	"github.com/spf13/cobra"
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

type stampTemplateVersionOptions struct {
	Repo string
	Ref  string
	SHA  string
	Env  string
	Now  string
}

func ciStampTemplateVersionCmd() *cobra.Command {
	var opts stampTemplateVersionOptions
	c := &cobra.Command{
		Use:   "stamp-template-version",
		Short: "write .template-version provenance for an instance checkout",
		Long: "Writes .template-version in the current repository, recording the template\n" +
			"repo/ref/commit an instance was generated from. With no explicit flags it\n" +
			"uses the same inference path as llz env add / llz upgrade; CI callers can\n" +
			"pass --repo/--ref/--sha for a throwaway instance render.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return stampTemplateVersionWithOptions(opts) },
	}
	c.Flags().StringVar(&opts.Repo, "repo", "", "template repo owner/name or URL (default: copier answers, upstream/origin remote, or akamai-consulting/lke-landing-zone)")
	c.Flags().StringVar(&opts.Ref, "ref", "", "template ref to record (default: git describe --tags --always, else current branch)")
	c.Flags().StringVar(&opts.SHA, "sha", "", "template commit SHA to record (default: git rev-parse HEAD)")
	c.Flags().StringVar(&opts.Env, "env", "", "deployment name to record informationally")
	c.Flags().StringVar(&opts.Now, "now", "", "timestamp override for reproducible tests (default: current UTC RFC3339 without fractional seconds)")
	return c
}

// stampTemplateVersion writes .template-version at the repo root, recording which
// template repo/ref/commit this instance was generated from. env is recorded
// informationally; if empty, the env from an existing stamp is preserved.
func stampTemplateVersion(env string) error {
	return stampTemplateVersionWithOptions(stampTemplateVersionOptions{Env: env})
}

func stampTemplateVersionWithOptions(opts stampTemplateVersionOptions) error {
	tv := templateVersion{Schema: 1, Generator: "llz", Env: opts.Env}

	if opts.Repo != "" {
		tv.TemplateRepo = normalizeTemplateRepo(opts.Repo)
	}
	if opts.SHA != "" {
		tv.TemplateSHA = opts.SHA
	}
	if opts.Ref != "" {
		tv.TemplateRef = opts.Ref
	}

	if tv.TemplateRepo == "" || tv.TemplateSHA == "" || tv.TemplateRef == "" {
		if a, _ := readAnswers("."); a != nil {
			if tv.TemplateRepo == "" {
				tv.TemplateRepo = normalizeTemplateRepo(a.SrcPath)
			}
			if tv.TemplateSHA == "" {
				tv.TemplateSHA = a.Commit
			}
			if tv.TemplateRef == "" {
				tv.TemplateRef = a.Commit
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
	// Preserve the first-seen env when none was passed.
	if tv.Env == "" {
		if b, err := os.ReadFile(".template-version"); err == nil {
			var prev templateVersion
			if json.Unmarshal(b, &prev) == nil {
				tv.Env = prev.Env
			}
		}
	}
	if opts.Now != "" {
		tv.StampedAt = opts.Now
	} else {
		tv.StampedAt = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	}

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
