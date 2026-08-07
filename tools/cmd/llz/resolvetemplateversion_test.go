package main

// The ResolveTemplateVersion tests STAYED in package main: they drive
// sustainDeps(), which is the CLI's wiring for internal/sustain. They were swept
// into the configreadiness move by an over-greedy extraction regex — the same
// mistake three times this session, and the reason to prefer line ranges over
// non-greedy matches when a file has several same-shaped functions.

import (
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/sustain"
)

// Provenance is DERIVED, never stamped to disk: with no .copier-answers.yml the
// repo falls back to the default and HEAD/describe come from git.
func TestResolveTemplateVersionFallsBackToGit(t *testing.T) {
	chdirTemp(t)
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		j := strings.Join(args, " ")
		switch {
		case strings.Contains(j, "rev-parse"):
			return []byte("deadbeefcafe1234\n"), nil
		case strings.Contains(j, "describe"):
			return []byte("v1.2.3\n"), nil
		default: // remote get-url -> empty
			return []byte(""), nil
		}
	})

	tv := sustain.ResolveTemplateVersion(sustainDeps())
	if tv.Schema != 1 || tv.Generator != "llz" {
		t.Errorf("resolved meta wrong: %+v", tv)
	}
	if tv.TemplateRepo != sustain.DefaultTemplateRepo {
		t.Errorf("TemplateRepo = %q, want default %q", tv.TemplateRepo, sustain.DefaultTemplateRepo)
	}
	if tv.TemplateSHA != "deadbeefcafe1234" || tv.TemplateRef != "v1.2.3" {
		t.Errorf("git fallback not used: %+v", tv)
	}
	if _, err := os.Stat(".template-version"); !os.IsNotExist(err) {
		t.Errorf("resolving provenance must not write a stamp file; stat err = %v", err)
	}
}

// copier's answers are the authority when present — no git calls needed.
func TestResolveTemplateVersionFromAnswers(t *testing.T) {
	chdirTemp(t)
	withExecOutput(t, func(_ string, _ ...string) ([]byte, error) {
		return []byte("unexpected-git-call\n"), nil
	})
	mustWrite(t, ".copier-answers.yml", "_commit: 1234567890abcdef\n_src_path: gh:akamai-consulting/lke-landing-zone\nllz_version: v9.9.9\n")

	tv := sustain.ResolveTemplateVersion(sustainDeps())
	if tv.TemplateRepo != sustain.DefaultTemplateRepo {
		t.Errorf("TemplateRepo = %q, want %q", tv.TemplateRepo, sustain.DefaultTemplateRepo)
	}
	if tv.TemplateSHA != "1234567890abcdef" || tv.TemplateRef != "v9.9.9" {
		t.Errorf("answers not honored: %+v", tv)
	}
}

// A not-yet-upgraded instance still carrying the retired stamp keeps working:
// the legacy file fills what the answers cannot.
func TestResolveTemplateVersionFallsBackToLegacyStamp(t *testing.T) {
	chdirTemp(t)
	withExecOutput(t, func(_ string, _ ...string) ([]byte, error) { return []byte(""), nil })
	mustWrite(t, ".template-version", `{"schema":1,"template_repo":"myorg/lke-landing-zone","template_ref":"v0.0.27","template_sha":"abc1234567"}`)

	tv := sustain.ResolveTemplateVersion(sustainDeps())
	if tv.TemplateRepo != "myorg/lke-landing-zone" || tv.TemplateRef != "v0.0.27" || tv.TemplateSHA != "abc1234567" {
		t.Errorf("legacy stamp not honored: %+v", tv)
	}
}
