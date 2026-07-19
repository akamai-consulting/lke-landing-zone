package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// freshFixture lays out a minimal scaffold: one managed vendored workflow, one
// managed composite action, one merge-classed caller stub (which must NOT be
// locked, since it carries per-instance tokens), and one file outside .github/.
func freshFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".template-manifest"),
		"merge    .github/workflows/**\n"+
			"managed  .github/workflows/llz-*.yml\n"+
			"managed  .github/actions/**\n"+
			"managed  README.md\n")
	writeFile(t, filepath.Join(dir, ".github/workflows/llz-terraform.yml"), "on: workflow_call\n")
	writeFile(t, filepath.Join(dir, ".github/workflows/terraform.yml"), "uses: ./.github/workflows/llz-terraform.yml\n")
	writeFile(t, filepath.Join(dir, ".github/actions/cluster-access/action.yml"), "runs:\n  using: composite\n")
	writeFile(t, filepath.Join(dir, "README.md"), "# instance\n")
	chdir(t, dir)
	return dir
}

func TestWorkflowsFreshLocksOnlyManagedGithubFiles(t *testing.T) {
	dir := freshFixture(t)
	if err := runWorkflowsFresh("", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, err := readVendoredLock(filepath.Join(dir, vendoredLockPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".github/workflows/llz-terraform.yml", ".github/actions/cluster-access/action.yml"} {
		if _, ok := got[want]; !ok {
			t.Errorf("managed .github file %s missing from lock", want)
		}
	}
	// The merge-classed caller stub carries instance tokens — locking it would
	// make every instance fail the guard.
	if _, ok := got[".github/workflows/terraform.yml"]; ok {
		t.Error("merge-classed caller stub must not be locked")
	}
	// Managed, but outside the vendored CI surface this guard covers.
	if _, ok := got["README.md"]; ok {
		t.Error("README.md is outside .github/ and must not be locked")
	}
	if len(got) != 2 {
		t.Errorf("lock has %d entries, want 2: %v", len(got), got)
	}
}

func TestWorkflowsFreshPassesCleanAndFailsOnEdit(t *testing.T) {
	dir := freshFixture(t)
	if err := runWorkflowsFresh("", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := runWorkflowsFresh("", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("clean scaffold should pass: %v", err)
	}

	body := filepath.Join(dir, ".github/workflows/llz-terraform.yml")
	writeFile(t, body, "on: workflow_call\n# operator edit\n")
	err := runWorkflowsFresh("", false, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("a hand-edited vendored body must fail the guard")
	}
	if !strings.Contains(err.Error(), "drifted") {
		t.Errorf("error should name the drift, got %v", err)
	}

	// A deleted vendored file is drift too — otherwise `rm` would silently pass.
	if err := os.Remove(body); err != nil {
		t.Fatal(err)
	}
	if err := runWorkflowsFresh("", false, io.Discard, io.Discard); err == nil {
		t.Fatal("a deleted vendored file must fail the guard")
	}
}

// Editing a merge-classed caller stub is legitimate — instances tune dispatch
// defaults there — so the guard must stay quiet about it.
func TestWorkflowsFreshIgnoresCallerStubEdits(t *testing.T) {
	dir := freshFixture(t)
	if err := runWorkflowsFresh("", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".github/workflows/terraform.yml"), "uses: ./.github/workflows/llz-terraform.yml\n# instance pin\n")
	if err := runWorkflowsFresh("", false, io.Discard, io.Discard); err != nil {
		t.Errorf("caller-stub edit must not trip the guard: %v", err)
	}
}

// A token-bearing file cannot be digest-locked (its rendered bytes differ per
// instance), so --write must refuse rather than ship a lock no instance can pass.
func TestWorkflowsFreshWriteRejectsTokenBearingManagedFile(t *testing.T) {
	dir := freshFixture(t)
	writeFile(t, filepath.Join(dir, ".github/workflows/llz-terraform.yml"), "image: <@ upstream_org @>/llz\n")
	err := runWorkflowsFresh("", true, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("--write must reject a managed .github file carrying a copier token")
	}
	if !strings.Contains(err.Error(), "copier token") {
		t.Errorf("error should explain the token problem, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, vendoredLockPath)); statErr == nil {
		t.Error("--write must not leave a lock behind when it rejects")
	}
}

// Instances rendered before the lock existed must keep linting cleanly.
func TestWorkflowsFreshSkipsWhenNoLock(t *testing.T) {
	freshFixture(t)
	if err := runWorkflowsFresh("", false, io.Discard, io.Discard); err != nil {
		t.Errorf("missing lock should skip, not fail: %v", err)
	}
}
