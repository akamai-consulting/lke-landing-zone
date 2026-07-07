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
