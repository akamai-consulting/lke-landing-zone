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
// locked, since it carries per-instance tokens), and one managed file outside
// .github/ (which MUST be locked — the lock follows the manifest, not a prefix).
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

// The lock covers every digest-locked class in the manifest, wherever it lives.
// It used to be scoped to a hardcoded `.github/` prefix, which left more than
// half the managed surface (lint configs, apl-values, the examples) overwritten
// by `llz upgrade` with no drift detection at all.
func TestManagedFreshLocksEveryManagedFileNotJustGithub(t *testing.T) {
	dir := freshFixture(t)
	if err := runManagedFresh("", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, err := readManagedLock(filepath.Join(dir, managedLockPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		".github/workflows/llz-terraform.yml",
		".github/actions/cluster-access/action.yml",
		"README.md", // managed and token-free, but OUTSIDE .github/
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("managed file %s missing from lock", want)
		}
	}
	// The merge-classed caller stub carries instance tokens — locking it would
	// make every instance fail the guard.
	if _, ok := got[".github/workflows/terraform.yml"]; ok {
		t.Error("merge-classed caller stub must not be locked")
	}
	// The lock is itself `managed`; locking it would race its own bytes.
	if _, ok := got[managedLockPath]; ok {
		t.Errorf("%s must not lock itself", managedLockPath)
	}
	if len(got) != 3 {
		t.Errorf("lock has %d entries, want 3: %v", len(got), got)
	}
}

func TestManagedFreshPassesCleanAndFailsOnEdit(t *testing.T) {
	dir := freshFixture(t)
	if err := runManagedFresh("", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := runManagedFresh("", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("clean scaffold should pass: %v", err)
	}

	body := filepath.Join(dir, ".github/workflows/llz-terraform.yml")
	writeFile(t, body, "on: workflow_call\n# operator edit\n")
	err := runManagedFresh("", false, io.Discard, io.Discard)
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
	if err := runManagedFresh("", false, io.Discard, io.Discard); err == nil {
		t.Fatal("a deleted vendored file must fail the guard")
	}
}

// Editing a merge-classed caller stub is legitimate — instances tune dispatch
// defaults there — so the guard must stay quiet about it.
func TestManagedFreshIgnoresCallerStubEdits(t *testing.T) {
	dir := freshFixture(t)
	if err := runManagedFresh("", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".github/workflows/terraform.yml"), "uses: ./.github/workflows/llz-terraform.yml\n# instance pin\n")
	if err := runManagedFresh("", false, io.Discard, io.Discard); err != nil {
		t.Errorf("caller-stub edit must not trip the guard: %v", err)
	}
}

// A token-bearing file cannot be digest-locked — its rendered bytes differ per
// instance — but it is still legitimately `managed`: `llz upgrade` overwrites it
// from a clean render, which substitutes that instance's own tokens. So it is
// omitted from the digests and DECLARED in the header, rather than rejected.
//
// (Rejecting used to be right only because the old `.github/` scope happened to
// contain no tokenful managed file. Widening the scope to the whole manifest
// makes AGENTS.md / README.md / .template-manifest legitimate members.)
func TestManagedFreshRecordsTokenBearingFilesAsDeclaredExclusions(t *testing.T) {
	dir := freshFixture(t)
	writeFile(t, filepath.Join(dir, "README.md"), "# <@ instance_repo @>\n")
	if err := runManagedFresh("", true, io.Discard, io.Discard); err != nil {
		t.Fatalf("a tokenful managed file must not fail --write: %v", err)
	}
	got, err := readManagedLock(filepath.Join(dir, managedLockPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["README.md"]; ok {
		t.Error("a tokenful file cannot be digest-locked — its bytes are per-instance")
	}
	raw, err := os.ReadFile(filepath.Join(dir, managedLockPath))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "DELIBERATELY UNLOCKED") || !strings.Contains(string(raw), "README.md") {
		t.Errorf("the gap must be declared in the header, not silently absent:\n%s", raw)
	}
	// Verification must still pass — the exclusion is deliberate, not drift.
	if err := runManagedFresh("", false, io.Discard, io.Discard); err != nil {
		t.Errorf("declared exclusions must not trip the guard: %v", err)
	}
}

// The verify pass walks the lock's own keys, so an unlocked managed file is
// invisible to it — the lock decays into a stale subset of the manifest. In the
// template repo (where copier.yml sits beside the scaffold) completeness is
// therefore checked explicitly, which is what makes the lock a projection of
// .template-manifest rather than a second, independently-maintained list.
func TestManagedFreshCatchesAManagedFileMissingFromTheLock(t *testing.T) {
	// Mirror the real repo layout: copier.yml at the root, scaffold beneath it.
	// That adjacency is what marks this as the TEMPLATE repo rather than a
	// rendered instance.
	root := t.TempDir()
	scaffold := filepath.Join(root, "instance-template")
	writeFile(t, filepath.Join(root, "copier.yml"), "_skip_if_exists: []\n")
	writeFile(t, filepath.Join(scaffold, ".template-manifest"), "managed  .github/actions/**\n")
	writeFile(t, filepath.Join(scaffold, ".github/actions/cluster-access/action.yml"), "runs:\n  using: composite\n")
	chdir(t, root)

	if err := runManagedFresh("instance-template", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := runManagedFresh("instance-template", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("a complete lock should pass: %v", err)
	}

	// A new managed file lands and nobody regenerates the lock.
	writeFile(t, filepath.Join(scaffold, ".github/actions/new-thing/action.yml"), "runs:\n  using: composite\n")
	err := runManagedFresh("instance-template", false, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("a managed file absent from the lock must fail in the template repo")
	}
	if !strings.Contains(err.Error(), "missing from the lock") {
		t.Errorf("error should name the completeness failure, got %v", err)
	}

	// Regenerating closes it.
	if err := runManagedFresh("instance-template", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := runManagedFresh("instance-template", false, io.Discard, io.Discard); err != nil {
		t.Errorf("regenerating the lock should restore the pass: %v", err)
	}
}

// An INSTANCE is rendered output, not the template: holding it to the
// completeness invariant would fail every instance whose shipped lock predates a
// newly-classified file. Only the template repo (copier.yml present) is checked.
func TestManagedFreshCompletenessIsTemplateRepoOnly(t *testing.T) {
	dir := freshFixture(t) // no copier.yml → instance context
	if err := runManagedFresh("", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".github/actions/new-thing/action.yml"), "runs:\n  using: composite\n")
	if err := runManagedFresh("", false, io.Discard, io.Discard); err != nil {
		t.Errorf("an instance must not fail on an unlocked managed file: %v", err)
	}
}

// Instances rendered before the lock existed must keep linting cleanly.
func TestManagedFreshSkipsWhenNoLock(t *testing.T) {
	freshFixture(t)
	if err := runManagedFresh("", false, io.Discard, io.Discard); err != nil {
		t.Errorf("missing lock should skip, not fail: %v", err)
	}
}
