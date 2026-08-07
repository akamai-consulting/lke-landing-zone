package newinstance

// helpers_test.go — the four helpers the moved tests reach for, written local and
// minimal rather than copied.
//
// withExecOutput SWAPS BOTH SEAMS, and the first draft of this file swapped only
// one. The comment there argued this package "never touches kubectlprobe — it
// shells out to git and gh only". That is false, and four tests said so
// immediately: `ghcli.OwnerKind` and `onboard.RepoStatus` reach the shell through
// `kubectlprobe.Exec`, so a stub that misses it leaves them running real `gh api`
// calls against the network. Same reason main's version swaps the pair.

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	prevExec, prevProbe := Exec, kubectlprobe.Exec
	Exec, kubectlprobe.Exec = fn, fn
	t.Cleanup(func() { Exec, kubectlprobe.Exec = prevExec, prevProbe })
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
	real := func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).Output()
	}
	Exec, kubectlprobe.Exec = real, real
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
