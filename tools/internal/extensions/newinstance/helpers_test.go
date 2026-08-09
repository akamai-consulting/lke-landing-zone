package newinstance

// helpers_test.go — the four helpers the moved tests reach for, written local and
// minimal rather than copied.
//
// withExecOutput SWAPS kubectlprobe.Exec, WHICH IS THE ONLY SEAM. It briefly was
// not: this package took its own injected `Exec` var, and the first draft stubbed
// that alone, arguing in a comment that the package "never touches kubectlprobe —
// it shells out to git and gh only". Four tests said otherwise on the spot,
// because `ghcli.OwnerKind` and `onboard.RepoStatus` reach the shell through
// kubectlprobe.Exec. The injected var is gone now and there is one seam again,
// which is the state that made the wrong comment impossible to write.

import (
	"errors"
	"io"
	"os"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	prev := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = prev })
}

// TestMain installs the two seams package main installs. Without it the tests
// that DON'T stub Exec — the ones running real git in a temp dir — dereference a
// nil func rather than failing an assertion, which reads as a crash in the code
// under test.
//
// The stand-in Exec deliberately does NOT reproduce main's stderr-attaching
// wrapper. That wrapper exists so an operator sees why `gh repo create` failed;
// no test here asserts on it, and a second copy of a scar is how the two drift.
func TestMain(m *testing.M) {
	InstallHooks = func(bool, bool, string) error { return nil }
	os.Exit(m.Run())
}

// withLookPath stubs the ONE PATH seam. kubectlprobe.LookPathFn is the single
// authority — package main's execLookPath delegates to it rather than owning a
// second var — so this package stubs it directly and skips the delegation.
func withLookPath(t *testing.T, fn func(file string) (string, error)) {
	t.Helper()
	orig := kubectlprobe.LookPathFn
	kubectlprobe.LookPathFn = fn
	t.Cleanup(func() { kubectlprobe.LookPathFn = orig })
}

// withCopierInstalled makes the copier preflight pass. `llz new` refuses before
// any GitHub round-trip when copier is absent, so every test of what comes AFTER
// that check has to satisfy it first.
func withCopierInstalled(t *testing.T) {
	t.Helper()
	withLookPath(t, func(f string) (string, error) { return "/usr/bin/" + f, nil })
}

// withoutCopier is the mirror: copier missing, everything else present. The pair
// exists because the interesting assertion is that `llz new` fails on the LOCAL
// check before spending two GitHub API calls to reach the same dead end.
func withoutCopier(t *testing.T) {
	t.Helper()
	withLookPath(t, func(f string) (string, error) {
		if f == "copier" {
			return "", errors.New(`exec: "copier": executable file not found in $PATH`)
		}
		return "/usr/bin/" + f, nil
	})
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// captureStderr collects what fn wrote to stderr. Most of this package's output
// IS the deliverable — the remediation text a failed `gh repo create` prints is
// the whole reason those error builders exist — so there is nothing else to
// assert on.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	defer func() { os.Stderr = orig }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	defer func() { os.Stdout = orig }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}
