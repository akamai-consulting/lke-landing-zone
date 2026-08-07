package buildpreflight

// seams_test.go — a MINIMAL local withExecOutput, written rather than copied.
//
// Seventh occurrence: package main's version carries installConfigReadinessDeps
// and a second kubectlprobe.Exec var that only exist over there.

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
