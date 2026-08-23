package templatecommit

import (
	"os"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// Helpers the moved tests use, copied across the boundary.

// stubTemplateCommit replaces the tag→commit round-trip for the duration of a
// test. Every test in this file installs one: without it a non-SHA ref would send
// a real request to api.github.com, which is both slow and a hermeticity break.
func stubTemplateCommit(t *testing.T, fn func(repo, ref string) (string, bool)) {
	t.Helper()
	prev := Resolve
	t.Cleanup(func() { Resolve = prev })
	Resolve = fn
}

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

// withExecOutput stubs the ONE seam; a minimal local version, because the copied
// one drags installConfigReadinessDeps.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = orig })
}
