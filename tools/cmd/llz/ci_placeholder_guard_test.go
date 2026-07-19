package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPlaceholderGuardCleanTree(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "chart-a.yaml", "apiVersion: v1\nkind: ConfigMap\ndata:\n  host: real.example.org\n")
	if err := runCIPlaceholderGuard(dir); err != nil {
		t.Fatalf("clean tree should pass, got: %v", err)
	}
}

func TestPlaceholderGuardFindsUnsubstitutedHost(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "chart-a.yaml", "apiVersion: v1\nkind: ConfigMap\ndata:\n  host: real.example.org\n")
	writeManifest(t, dir, "chart-b.yaml", "apiVersion: v1\nkind: Ingress\nspec:\n  rules:\n    - host: api.placeholder.example.com\n")
	err := runCIPlaceholderGuard(dir)
	if err == nil {
		t.Fatal("a surviving placeholder host must fail the guard")
	}
	if !strings.Contains(err.Error(), "1 unsubstituted") {
		t.Errorf("error should count the findings, got: %v", err)
	}
}

// The whole reason this guard fails closed: the shell version it replaced used
// `grep -r`, whose non-zero exit on an empty/missing tree read as "no
// placeholders found" — the same clean pass as a fully-rendered tree with none.
func TestPlaceholderGuardFailsOnEmptyCorpus(t *testing.T) {
	dir := t.TempDir() // exists but holds no manifests
	err := runCIPlaceholderGuard(dir)
	if err == nil {
		t.Fatal("an empty corpus must fail, not report a vacuous pass")
	}
	if !strings.Contains(err.Error(), "examined 0 manifest files") {
		t.Errorf("error should name the empty-corpus cause, got: %v", err)
	}
}

func TestPlaceholderGuardFailsOnMissingRenderDir(t *testing.T) {
	err := runCIPlaceholderGuard(filepath.Join(t.TempDir(), "never-rendered"))
	if err == nil {
		t.Fatal("a missing render dir must fail (the tree was never rendered)")
	}
}

func TestCollectPlaceholderFindingsReportsLineNumbers(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "c.yaml", "a: 1\nb: placeholder.example.com\nc: 3\nd: x.placeholder.example.com\n")
	findings, examined, err := collectPlaceholderFindings([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if examined != 1 {
		t.Errorf("examined = %d, want 1", examined)
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %d, want 2", len(findings))
	}
	if findings[0].line != 2 || findings[1].line != 4 {
		t.Errorf("line numbers = %d,%d, want 2,4", findings[0].line, findings[1].line)
	}
	if findings[0].text != "b: placeholder.example.com" {
		t.Errorf("text = %q", findings[0].text)
	}
}

// Both manifest extensions must be scanned — the guard-walk divergence that
// guard_walk.go exists to prevent (a *.yml manifest policed by some guards and
// invisible to others).
func TestPlaceholderGuardScansYmlToo(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "c.yml", "host: placeholder.example.com\n")
	findings, _, err := collectPlaceholderFindings([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Errorf("a .yml manifest must be scanned, findings = %d", len(findings))
	}
}

// An absolute --render-dir must survive the --root join: filepath.Join(".",
// "/abs") cleans to "abs", which would silently retarget the scan at a relative
// path that does not exist and surface as a bogus empty-corpus failure.
func TestPlaceholderGuardAcceptsAbsoluteRenderDir(t *testing.T) {
	dir := t.TempDir() // t.TempDir() is absolute
	writeManifest(t, dir, "c.yaml", "host: placeholder.example.com\n")
	cmd := ciPlaceholderGuardCmd()
	cmd.SetArgs([]string{"--render-dir", dir})
	if err := cmd.Execute(); err == nil {
		t.Fatal("absolute --render-dir should have been scanned and found the placeholder")
	} else if strings.Contains(err.Error(), "examined 0") {
		t.Fatalf("absolute path was mangled by the --root join: %v", err)
	}
}
