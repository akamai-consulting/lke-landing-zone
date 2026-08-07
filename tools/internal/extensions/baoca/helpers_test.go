package baoca

// helpers_test.go — the seams these tests must swap.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// withExecOutput swaps the seam THIS package's read path takes.
//
// It is kubectlprobe.Exec — the extract verb shells out to `kubectl get secret`.
// Package main's helper of the same name reinstalls configreadiness's Deps, which
// is irrelevant here and would drag an unrelated install across the boundary to
// satisfy a name; the fifth package to need that correction.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	prev := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = prev })
}
