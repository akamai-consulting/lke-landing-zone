package main

// Gap-closing tests for checks.go: the binary-file skip in the conflict-marker
// scan, the line numbers the dropped-apiVersion guard reports, and the one
// property both gate runners exist for — a failing step FAILS the gate.

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
)

// A NUL anywhere means "binary, skip it" — including at offset 0, which is where
// it lands in the formats that most often carry marker-shaped bytes (compiled
// objects, images). Scanning one of those reports conflict markers in a file no
// human ever merged, and lint fails with nothing to resolve.
func TestStepConflictMarkersSkipsAFileWhoseFirstByteIsNUL(t *testing.T) {
	dir := t.TempDir()
	// Marker lines are real; the leading NUL is what makes the file binary.
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"),
		[]byte("\x00\n<<<<<<< HEAD\na\n=======\nb\n>>>>>>> other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	// `git ls-files` tracks it; the file itself is read off disk.
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "ls-files") {
			return []byte("bin.dat\n"), nil
		}
		return nil, errors.New("unexpected git call")
	})
	if err := stepConflictMarkers(cliopts.Global); err != nil {
		t.Fatalf("a file whose first byte is NUL is binary and must be skipped, got: %v", err)
	}
}

// The report is a `file:line` an operator (and the ::error annotation) jumps to.
// An off-by-one sends them to the wrong line — or to line 0/-1, which no editor
// or PR annotation can resolve at all.
func TestScanDroppedAPIVersionsReportsTheDeclaringLine(t *testing.T) {
	root := t.TempDir()
	rel := filepath.Join("kubernetes-custom", "es.yaml")
	if err := os.MkdirAll(filepath.Join(root, "kubernetes-custom"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The dropped apiVersion is on line 3 (1-based).
	body := "# a comment\n---\napiVersion: external-secrets.io/v1beta1\nkind: ExternalSecret\n"
	if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	hits, examined, err := scanDroppedAPIVersions(root)
	if err != nil {
		t.Fatal(err)
	}
	if examined == 0 {
		t.Fatal("examined = 0: the scan read nothing, so a clean result would be meaningless")
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want exactly one", hits)
	}
	if want := "kubernetes-custom/es.yaml:3"; hits[0].loc != want {
		t.Errorf("hit loc = %q, want %q (1-based line of the declaration)", hits[0].loc, want)
	}
}

// ── the gate runners ─────────────────────────────────────────────────────────

// runLint's entire contract is that a failing step fails the gate. Inverting the
// error test makes `llz lint` (and the pre-commit hook, and every instance's CI)
// pass while a check is actively reporting a problem — the silent-breakage class
// these guards exist to close.
func TestRunLintFailsWhenAStepFails(t *testing.T) {
	chdir(t, t.TempDir())
	// stepConflictMarkers is first: `ls-files` failing INSIDE a repo is its hard
	// error (as opposed to "not a repo", which is a skip).
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "ls-files"):
			return nil, errors.New("fatal: index file corrupt")
		case strings.Contains(joined, "rev-parse"):
			return []byte(".git\n"), nil
		}
		return nil, errors.New("unexpected git " + joined)
	})

	var err error
	out := captureStderr(t, func() { err = runLint(globalOpts{}) })
	if err == nil {
		t.Fatal("runLint = nil while a step reported a problem — the gate would pass a broken tree")
	}
	if !strings.Contains(err.Error(), "refusing to report clean") {
		t.Errorf("runLint should surface the step's own error, got: %v", err)
	}
	if strings.Contains(out, "lint: ok") {
		t.Errorf("a failed run must not print the all-clear:\n%s", out)
	}
}

// The same contract for runValidate. LLZ_TERRAFORM points the step at a script
// that exits non-zero, so nothing real is executed and no network is touched.
func TestRunValidateFailsWhenAStepFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "terraform-iac-bootstrap", "cluster"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLZ_TERRAFORM", writeExitScript(t, dir, "tf-fails", 1))
	// Keep the second step cheap and deterministic whatever is installed locally.
	t.Setenv("LLZ_CHECKOV", writeExitScript(t, dir, "checkov-passes", 0))
	chdir(t, dir)

	var err error
	out := captureStderr(t, func() { err = runValidate(globalOpts{}) })
	if err == nil {
		t.Fatal("runValidate = nil while terraform validate failed — the gate would pass an invalid root")
	}
	if strings.Contains(out, "validate: ok") {
		t.Errorf("a failed run must not print the all-clear:\n%s", out)
	}
}

// writeExitScript drops an executable that exits with the given code and returns
// its absolute path (so haveTool/LookPath resolves it without touching PATH).
func writeExitScript(t *testing.T, dir, name string, code int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	body := "#!/bin/sh\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
	}
	return p
}
