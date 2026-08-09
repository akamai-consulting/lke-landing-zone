package mutate

// helpers_test.go — the seam swap the moved tests need.

import "testing"

// withExecOutput replaces the one live shell-out this package has.
//
// Package main's helper of the same name also reinstalls configreadiness's
// capabilities, because in main that seam is shared with half a dozen verbs. None
// of that applies here, and copying the original wholesale would drag an unrelated
// Deps install across the boundary to satisfy a name — the same correction
// internal/argodiag needed.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	prev := execOutput
	execOutput = fn
	t.Cleanup(func() { execOutput = prev })
}
