package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// In the template repo the scaffold lives under instance-template/, and the
// lock is part of that scaffold — it has to be written (and read back) beside
// the .template-manifest it describes, not in the repo root. The cwd-relative
// spelling is reserved for a rendered instance, whose root IS ".".
func TestWorkflowsFreshLockLivesBesideTheDetectedScaffoldRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "instance-template")
	writeFile(t, filepath.Join(root, ".template-manifest"), "managed  .github/workflows/llz-*.yml\n")
	writeFile(t, filepath.Join(root, ".github/workflows/llz-terraform.yml"), "on: workflow_call\n")
	chdir(t, dir)

	if err := runWorkflowsFresh("", true, io.Discard, io.Discard); err != nil {
		t.Fatalf("--write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, vendoredLockPath)); err != nil {
		t.Fatalf("the lock must be written under the detected scaffold root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, vendoredLockPath)); err == nil {
		t.Error("the lock must not land in the cwd when the scaffold root is instance-template/")
	}
	// And the check pass must read it back from the same place.
	if err := runWorkflowsFresh("", false, io.Discard, io.Discard); err != nil {
		t.Errorf("a freshly written lock must verify clean: %v", err)
	}
}

// Missing and drifted files are two shapes of the same failure and the guard
// reports ONE count over both. Netting them against each other reports "0
// template-owned file(s) drifted" on a tree where two files are wrong.
func TestWorkflowsFreshCountsMissingAndDriftedTogether(t *testing.T) {
	dir := freshFixture(t)
	if err := runWorkflowsFresh("", true, io.Discard, io.Discard); err != nil {
		t.Fatalf("--write: %v", err)
	}
	writeFile(t, filepath.Join(dir, ".github/workflows/llz-terraform.yml"), "on: workflow_call\n# operator edit\n")
	if err := os.Remove(filepath.Join(dir, ".github/actions/cluster-access/action.yml")); err != nil {
		t.Fatal(err)
	}

	var errOut strings.Builder
	err := runWorkflowsFresh("", false, io.Discard, &errOut)
	if err == nil {
		t.Fatal("one edited + one deleted template-owned file must fail the guard")
	}
	if !strings.Contains(err.Error(), "2 template-owned file(s) drifted") {
		t.Errorf("the error must count both, got %v", err)
	}
	if !strings.Contains(errOut.String(), "2 template-owned file(s) drifted from the template") {
		t.Errorf("the report must count both:\n%s", errOut.String())
	}
}

// The lock parser points at the offending line so a hand-edited lock is fixable
// without counting lines by hand.
func TestReadVendoredLockNamesTheOffendingLineNumber(t *testing.T) {
	p := filepath.Join(t.TempDir(), vendoredLockPath)
	writeFile(t, p, "# GENERATED — do not hand-edit\nnot-a-valid-entry\n")

	_, err := readVendoredLock(p)
	if err == nil {
		t.Fatal("a malformed entry must be rejected")
	}
	if !strings.Contains(err.Error(), ":2 bad entry") {
		t.Errorf("error must name line 2, got %v", err)
	}
}
