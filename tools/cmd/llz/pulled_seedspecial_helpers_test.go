package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Helpers the moved tests use, copied across the new package boundary.

// chdirTempDir moves the test into a fresh temp dir (the commands resolve tfvars
// relative to the workflow's checkout root).
func chdirTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	return dir
}

func writeTFVars(t *testing.T, dir, sub, region, content string) {
	t.Helper()
	p := filepath.Join(dir, "terraform-iac-bootstrap", sub)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, region+".tfvars"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
