package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadAnswers(t *testing.T) {
	dir := t.TempDir()
	body := "" +
		"_commit: v0.0.38\n" +
		"_src_path: gh:akamai-consulting/lke-landing-zone\n" +
		"upstream_org: akamai-consulting\n" +
		"instance_repo: my-org/my-instance\n"
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := readAnswers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a == nil {
		t.Fatal("expected answers, got nil")
	}
	if a.Commit != "v0.0.38" || a.UpstreamOrg != "akamai-consulting" || a.InstanceRepo != "my-org/my-instance" {
		t.Errorf("parsed answers: %+v", a)
	}
}

// pinnedTemplateRef is the ONE place the template pin is read now that the
// workflows carry no template-ref input, so its precedence is load-bearing:
// llz_version (what `llz upgrade` rewrites) beats _commit, both beat the legacy
// .template-version stamp, and an instance-less dir resolves to "".
func TestPinnedTemplateRef(t *testing.T) {
	write := func(t *testing.T, dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("llz_version wins", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, ".copier-answers.yml", "_commit: v0.0.31\nllz_version: v0.0.32\n")
		write(t, dir, ".template-version", `{"template_ref":"v0.0.30"}`)
		t.Chdir(dir)
		if got := pinnedTemplateRef(); got != "v0.0.32" {
			t.Errorf("pinnedTemplateRef() = %q, want v0.0.32", got)
		}
	})

	t.Run("falls back to _commit", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, ".copier-answers.yml", "_commit: v0.0.31\n")
		t.Chdir(dir)
		if got := pinnedTemplateRef(); got != "v0.0.31" {
			t.Errorf("pinnedTemplateRef() = %q, want v0.0.31", got)
		}
	})

	t.Run("legacy stamp is the last resort", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, ".template-version", `{"template_ref":"v0.0.30"}`)
		t.Chdir(dir)
		if got := pinnedTemplateRef(); got != "v0.0.30" {
			t.Errorf("pinnedTemplateRef() = %q, want v0.0.30", got)
		}
	})

	t.Run("no instance resolves empty", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if got := pinnedTemplateRef(); got != "" {
			t.Errorf("pinnedTemplateRef() = %q, want empty", got)
		}
	})
}

func TestReadAnswersMissingIsNil(t *testing.T) {
	a, err := readAnswers(t.TempDir())
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if a != nil {
		t.Errorf("expected nil answers, got %+v", a)
	}
}
