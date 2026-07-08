package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTemplateManifestCheckClassifyAndList(t *testing.T) {
	root := t.TempDir()
	scaffold := filepath.Join(root, "instance-template")
	writeTestFile(t, scaffold, ".template-manifest", ""+
		"managed **\n"+
		"merge docs/**\n"+
		"owned docs/local.md\n")
	writeTestFile(t, scaffold, "README.md", "readme\n")
	writeTestFile(t, scaffold, "docs/guide.md", "guide\n")
	writeTestFile(t, scaffold, "docs/local.md", "local\n")
	writeTestFile(t, filepath.Join(scaffold, ".terraform"), "ignored.tf", "ignored\n")

	var out, errOut bytes.Buffer
	if err := runTemplateManifest(scaffold, "", "", &out, &errOut); err != nil {
		t.Fatalf("check failed: %v\nstderr: %s", err, errOut.String())
	}
	for _, want := range []string{"managed=2", "merge=1", "owned=1", "4 files"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("check output %q missing %q", out.String(), want)
		}
	}

	out.Reset()
	errOut.Reset()
	if err := runTemplateManifest(scaffold, "docs/local.md", "", &out, &errOut); err != nil {
		t.Fatalf("classify failed: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "owned" {
		t.Fatalf("classify = %q, want owned", got)
	}

	out.Reset()
	if err := runTemplateManifest(scaffold, "", "merge", &out, &errOut); err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "docs/guide.md" {
		t.Fatalf("list merge = %q, want docs/guide.md", got)
	}
}

func TestTemplateManifestReportsUnclassifiedFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".template-manifest", "managed .template-manifest\nmanaged README.md\n")
	writeTestFile(t, root, "README.md", "readme\n")
	writeTestFile(t, root, "values.yaml", "x\n")

	var out, errOut bytes.Buffer
	err := runTemplateManifest(root, "", "", &out, &errOut)
	if err == nil {
		t.Fatal("expected unclassified-file error")
	}
	for _, want := range []string{"::error::1 scaffold file", "values.yaml"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr %q missing %q", errOut.String(), want)
		}
	}
}

func TestTemplateManifestCommandWiring(t *testing.T) {
	c := ciTemplateManifestCmd()
	for _, flag := range []string{"root", "classify", "list"} {
		if c.Flags().Lookup(flag) == nil {
			t.Fatalf("missing --%s flag", flag)
		}
	}
	if err := c.Args(c, []string{"extra"}); err == nil {
		t.Fatal("template-manifest accepted positional args")
	}
}

func TestTemplateManifestCopierConsistency(t *testing.T) {
	root := t.TempDir()
	scaffold := filepath.Join(root, "instance-template")
	writeTestFile(t, scaffold, ".template-manifest", ""+
		"managed **\n"+
		"owned keep.txt\n"+
		"owned .copier-answers.yml\n")
	writeTestFile(t, scaffold, "keep.txt", "instance content\n")
	writeTestFile(t, scaffold, ".copier-answers.yml", "_commit: v1\n")

	// copier.yml protecting NEITHER owned file: keep.txt is a violation, but
	// .copier-answers.yml is exempt (it is the _answers_file copier regenerates).
	writeTestFile(t, root, "copier.yml", "_answers_file: .copier-answers.yml\n_skip_if_exists: []\n_exclude: []\n")

	var out, errOut bytes.Buffer
	if err := runTemplateManifest(scaffold, "", "", &out, &errOut); err == nil {
		t.Fatalf("expected failure: keep.txt is owned but unprotected by copier\nstdout: %s", out.String())
	}
	if !strings.Contains(errOut.String(), "keep.txt") {
		t.Errorf("error should name the unprotected owned file: %s", errOut.String())
	}
	if strings.Contains(errOut.String(), ".copier-answers.yml") {
		t.Errorf("_answers_file must be exempt from the check: %s", errOut.String())
	}

	// Protect keep.txt via _skip_if_exists → now consistent.
	writeTestFile(t, root, "copier.yml", "_answers_file: .copier-answers.yml\n_skip_if_exists:\n  - \"keep.txt\"\n")
	out.Reset()
	errOut.Reset()
	if err := runTemplateManifest(scaffold, "", "", &out, &errOut); err != nil {
		t.Fatalf("should pass once keep.txt is protected: %v\nstderr: %s", err, errOut.String())
	}

	// _exclude also counts as protection.
	writeTestFile(t, root, "copier.yml", "_answers_file: .copier-answers.yml\n_exclude:\n  - \"keep.txt\"\n")
	out.Reset()
	errOut.Reset()
	if err := runTemplateManifest(scaffold, "", "", &out, &errOut); err != nil {
		t.Fatalf("_exclude should also satisfy the check: %v\nstderr: %s", err, errOut.String())
	}
}

func writeTestFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
