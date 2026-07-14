package main

import (
	"os"
	"os/exec"
	"testing"
)

func TestCurrentTemplateRef(t *testing.T) {
	t.Chdir(t.TempDir())

	// No .template-version → empty.
	if got := currentTemplateRef(); got != "" {
		t.Errorf("no file: got %q, want empty", got)
	}
	// template_ref wins.
	os.WriteFile(".template-version", []byte(`{"template_ref":"v0.1.2","template_sha":"abcdef1234567890"}`), 0o644)
	if got := currentTemplateRef(); got != "v0.1.2" {
		t.Errorf("ref: got %q, want v0.1.2", got)
	}
	// falls back to short SHA when ref is absent.
	os.WriteFile(".template-version", []byte(`{"template_sha":"abcdef1234567890"}`), 0o644)
	if got := currentTemplateRef(); got != "abcdef12" {
		t.Errorf("sha fallback: got %q, want abcdef12", got)
	}
	// malformed → empty (not a crash).
	os.WriteFile(".template-version", []byte(`not json`), 0o644)
	if got := currentTemplateRef(); got != "" {
		t.Errorf("malformed: got %q, want empty", got)
	}
}

func TestUpgradeConflictFiles(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	os.WriteFile("clean.yaml", []byte("a: 1\n"), 0o644)
	git("add", "-A")
	git("commit", "-qm", "base")

	// Clean tree → nothing flagged.
	if bad := upgradeConflictFiles(); len(bad) != 0 {
		t.Errorf("clean tree flagged %v", bad)
	}

	// A tracked file modified with a marker (unstaged) + a new untracked one — both caught.
	os.WriteFile("clean.yaml", []byte("a: 1\n<<<<<<< HEAD\nb: 2\n"), 0o644)
	os.WriteFile("added.yaml", []byte(">>>>>>> theirs\n"), 0o644)
	bad := upgradeConflictFiles()
	if len(bad) != 2 || bad[0] != "added.yaml" || bad[1] != "clean.yaml" {
		t.Errorf("got %v, want [added.yaml clean.yaml]", bad)
	}
}
