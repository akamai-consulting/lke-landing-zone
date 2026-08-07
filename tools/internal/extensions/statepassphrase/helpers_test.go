package statepassphrase

// helpers_test.go — the seam swap the moved tests need.

import (
	"io"
	"os"
	"strings"
	"testing"
)

// withExecOutput replaces the Exec capability for one test.
//
// Package main's helper of the same name also reinstalls configreadiness's
// capabilities, because there the seam is shared with half a dozen verbs. None of
// that applies here, and copying it wholesale would drag an unrelated Deps install
// across the boundary to satisfy a name — the correction internal/argodiag and
// internal/mutate both needed.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	prev := caps.Exec
	caps.Exec = fn
	t.Cleanup(func() { caps.Exec = prev })
}

// withGHJSONPaged replaces the paginated-GET capability for one test.
func withGHJSONPaged(t *testing.T, fn func(path string, out any) error) {
	t.Helper()
	prev := caps.GHJSONPaged
	caps.GHJSONPaged = fn
	t.Cleanup(func() { caps.GHJSONPaged = prev })
}

// captureStderr mirrors captureStdout for the os.Stderr path (the remediation /
// warning printers write there).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = orig
	var b strings.Builder
	if _, err := io.Copy(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
