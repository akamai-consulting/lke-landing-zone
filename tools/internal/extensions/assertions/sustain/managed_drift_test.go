package sustain

// managed_drift_test.go — the coupling between the lock WRITER and the drift
// READER, exercised through both real functions.
//
// The rule "does this file still match what the template shipped" now has two
// consumers: `managed-fresh` (which fails CI) and `llz upgrade` (which names the
// edits it discards). docs/e2e-gates.md's split-contract archetype says the way
// that breaks is each side growing its own copy — so the digest comparison lives
// once, in compareManagedLock, and this test feeds writeManagedLock's REAL output
// into DriftedManaged's REAL predicate rather than restating either.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// lockedScaffold builds a throwaway scaffold of `files`, writes a real lock over
// it with the real writer, and returns the root plus Deps pointed at it.
//
// The manifest lookup is the one thing faked: which classes are digest-locked is
// ADR 0014's question and not what this test is about. Everything downstream of
// the file list — the digest format, the lock file, the comparison — is real.
func lockedScaffold(t *testing.T, files map[string]string) (string, Deps) {
	t.Helper()
	root := t.TempDir()
	var rels []string
	for rel, body := range files {
		writeFile(t, filepath.Join(root, filepath.FromSlash(rel)), body)
		rels = append(rels, rel)
	}
	slices.Sort(rels)
	d := Deps{LockableScaffoldFiles: func(string) (string, []string, error) { return root, rels, nil }}
	if err := writeManagedLock(root, rels, filepath.Join(root, ManagedLockPath), os.Stderr); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	return root, d
}

// A freshly written lock must describe the tree it was written from. If this
// fails, every other assertion here is measuring the writer rather than the edit.
func TestDriftedManagedIsCleanOnTheTreeTheLockWasWrittenFrom(t *testing.T) {
	_, d := lockedScaffold(t, map[string]string{
		".github/workflows/llz-terraform.yml": "name: pipeline\n",
		".tflintrc.hcl":                       "config {}\n",
	})
	drift, want, err := DriftedManaged(d, "")
	if err != nil {
		t.Fatalf("DriftedManaged: %v", err)
	}
	if len(want) != 2 {
		t.Fatalf("lock covers %d file(s), want 2 — the writer did not record the scaffold", len(want))
	}
	if drift.Any() {
		t.Errorf("unedited tree reported as drifted: %+v", drift)
	}
}

// THE BEHAVIOR THE UPGRADE DEPENDS ON: an operator's edit to a template-owned
// file is visible before copier runs. Without this, reportClobberedManaged has
// nothing to report and the upgrade goes back to swallowing the edit silently.
func TestDriftedManagedSeesAnEditToALockedFile(t *testing.T) {
	root, d := lockedScaffold(t, map[string]string{
		".github/workflows/llz-terraform.yml": "name: pipeline\n",
		".tflintrc.hcl":                       "config {}\n",
	})
	edited := ".github/workflows/llz-terraform.yml"
	writeFile(t, filepath.Join(root, filepath.FromSlash(edited)), "name: pipeline\n# my local unblock\n")

	drift, _, err := DriftedManaged(d, "")
	if err != nil {
		t.Fatalf("DriftedManaged: %v", err)
	}
	if got := drift.Edited; len(got) != 1 || got[0] != edited {
		t.Errorf("Edited = %v, want exactly [%s]", got, edited)
	}
	if len(drift.Missing) != 0 {
		t.Errorf("Missing = %v, want none — the file is present, just changed", drift.Missing)
	}
	if !drift.Any() || len(drift.All()) != 1 {
		t.Errorf("Any()=%t All()=%v; want a single drifted path", drift.Any(), drift.All())
	}
}

// A deleted locked file is MISSING, not EDITED — the two get different remedies
// (restore it vs. recover your change), so collapsing them would send the
// operator to the wrong one.
func TestDriftedManagedSeparatesDeletedFromEdited(t *testing.T) {
	root, d := lockedScaffold(t, map[string]string{
		".github/workflows/llz-terraform.yml": "name: pipeline\n",
		".tflintrc.hcl":                       "config {}\n",
	})
	if err := os.Remove(filepath.Join(root, ".tflintrc.hcl")); err != nil {
		t.Fatal(err)
	}
	drift, _, err := DriftedManaged(d, "")
	if err != nil {
		t.Fatalf("DriftedManaged: %v", err)
	}
	if got := drift.Missing; len(got) != 1 || got[0] != ".tflintrc.hcl" {
		t.Errorf("Missing = %v, want [.tflintrc.hcl]", got)
	}
	if len(drift.Edited) != 0 {
		t.Errorf("Edited = %v, want none", drift.Edited)
	}
}

// A pre-lock instance has nothing to compare against. That must be "nothing to
// say" — not an error that fails an upgrade, and not a clean bill of health the
// caller could mistake for evidence.
func TestDriftedManagedOnAnInstanceWithNoLock(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".tflintrc.hcl"), "config {}\n")
	d := Deps{LockableScaffoldFiles: func(string) (string, []string, error) {
		return root, []string{".tflintrc.hcl"}, nil
	}}
	drift, want, err := DriftedManaged(d, "")
	if err != nil {
		t.Fatalf("a missing lock must not be an error: %v", err)
	}
	if want != nil {
		t.Errorf("lock = %v, want nil so the caller can tell 'no lock' from 'empty lock'", want)
	}
	if drift.Any() {
		t.Errorf("drift = %+v, want empty", drift)
	}
}

// A Deps with no LockableScaffoldFiles cannot answer, and must SAY so rather
// than crash. This is not hypothetical: the upgrade package's TestMain builds
// exactly that Deps, documented as safe because nothing reachable used the field —
// and the first caller that did made `llz upgrade` panic. An error keeps the
// advisory silent and keeps the guard loud; a panic takes the command down.
func TestDriftedManagedRejectsAnUnwiredDeps(t *testing.T) {
	_, _, err := DriftedManaged(Deps{}, "")
	if err == nil {
		t.Fatal("an unwired Deps must be an error, not a clean verdict")
	}
	if !strings.Contains(err.Error(), "LockableScaffoldFiles") {
		t.Errorf("error must name the missing seam, got %v", err)
	}
}
