package branchpolicy

// seams_test.go — a MINIMAL local withExecOutput.
//
// Written rather than copied: package main's version carries
// installConfigReadinessDeps and a second execOutput var that only exist over
// there. Sixth time a copied with*() helper would have dragged its old package's
// wiring along.
//
// It stubs kubectlprobe.Exec because that is the seam this package's execOutput
// delegates to — one seam, reached at call time.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = orig })
}
