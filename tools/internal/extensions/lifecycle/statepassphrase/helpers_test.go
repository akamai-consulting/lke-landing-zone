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

// tmpRootsDir returns a real, existing directory to pass as --roots-dir.
//
// A REAL ONE, not the literal "terraform" these tests used to pass. RunRotate
// now stats the roots DIRECTORY itself and refuses when it is absent, because
// that is how a wrong --roots-dir produced four "root not present" skips and an
// exit 0 licensing deletion of the old passphrase. Handing it a path that does
// not exist would make every test here take the new refusal arm — and stubbing
// the stat would remove the only unstubbed check standing between a typo and
// four unreadable state files. Whether the individual ROOTS exist is still
// decided by the statePassphraseRootExists seam, which is what each test drives.
func tmpRootsDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
