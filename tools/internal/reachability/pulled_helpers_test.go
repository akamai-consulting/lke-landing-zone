package reachability

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

// Helpers the moved tests use, copied across the new package boundary.

func withLookPath(t *testing.T, fn func(file string) (string, error)) {
	t.Helper()
	// The ONE seam: this package's execLookPath delegates to kubectlprobe.
	orig := kubectlprobe.LookPathFn
	kubectlprobe.LookPathFn = fn
	t.Cleanup(func() { kubectlprobe.LookPathFn = orig })
}

// chdirTempDir moves the test into a fresh temp dir (the commands resolve tfvars
// relative to the workflow's checkout root).
func chdirTempDir(t *testing.T) string {
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
	return dir
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// withExecOutput stubs the ONE seam this package takes.
//
// The version copied from package main carried installConfigReadinessDeps and a
// second execOutput var — machinery that only exists over there. Fifth time a
// copied with*() helper has dragged its old package's wiring along; a minimal
// local version is the answer every time.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = orig })
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote — these helpers print a human report we don't want in test output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig
	var b strings.Builder
	if _, err := io.Copy(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
