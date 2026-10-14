package onboard

// lookpath_test.go — the PATH seam stub for the doctor gate test that came from
// package main. kubectlprobe.LookPathFn is the one seam; this package's
// execLookPath delegates to it.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

func withLookPath(t *testing.T, fn func(file string) (string, error)) {
	t.Helper()
	orig := kubectlprobe.LookPathFn
	kubectlprobe.LookPathFn = fn
	t.Cleanup(func() { kubectlprobe.LookPathFn = orig })
}
