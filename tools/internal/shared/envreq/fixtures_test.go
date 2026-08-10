package envreq

// chdirTemp, copied not shared -- the same rule every other fixture in this
// campaign follows. The requirements model moved down here and its tests came
// with it; the helper stays in both places rather than putting `testing` in a
// production package.

import (
	"os"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// chdirTemp lived in driftrun_test.go, which moved to internal/sustain with the
// drift verb. Other package-main tests still use it.
func chdirTemp(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// withExecOutput swaps this package's Exec seam for one test.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	// SWAPS kubectlprobe.Exec, not a local Deps field. The seam this used to
	// replace was configreadiness's deps.Exec, which package main installed as
	// execOutput == kubectlprobe.Exec while the package default was a silent
	// (nil, nil) no-op. Collapsing that to the real seam is what let this file
	// move; the fixture now swaps the one implementation there is.
	orig := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = orig })
}
