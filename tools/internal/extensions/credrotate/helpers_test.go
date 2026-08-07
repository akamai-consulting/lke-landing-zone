package credrotate

// helpers_test.go — fixtures the moved tests need.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

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

// withGHSetSecret swaps the gh-secret seam, recording "name@env" calls.
func withGHSetSecret(t *testing.T, fail func(name string) error) *[]string {
	t.Helper()
	orig := SetSecret
	calls := new([]string)
	SetSecret = func(name, ghEnv, value string) error {
		*calls = append(*calls, name+"@"+ghEnv+"="+value)
		if fail != nil {
			return fail(name)
		}
		return nil
	}
	t.Cleanup(func() { SetSecret = orig })
	return calls
}

// captureFirewallOutput runs fn with os.Stdout and os.Stderr redirected to
// pipes and returns what it printed to each.
func captureFirewallOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wo, we
	fn()
	wo.Close()
	we.Close()
	o, _ := io.ReadAll(ro)
	e, _ := io.ReadAll(re)
	return string(o), string(e)
}

// captureStderr mirrors captureStdout for the os.Stderr path (the remediation /
// warning printers write there).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = orig
	var b strings.Builder
	if _, err := io.Copy(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// withGHAEnvFile captures $GITHUB_ENV writes; returns the path.
func withGHAEnvFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gha-env")
	t.Setenv("GITHUB_ENV", p)
	return p
}

// withKubectl stubs the kubectlprobe.Exec seam to answer kubectl invocations via a
// handler keyed on the joined args; non-kubectl shell-outs error. An unstubbed
// kubectl call returns an error, which the section helpers treat as "empty".
func withKubectl(t *testing.T, h func(args string) ([]byte, error)) {
	t.Helper()
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			return nil, fmt.Errorf("unexpected command %q", name)
		}
		return h(strings.Join(args, " "))
	})
}

// withExecOutput swaps the kubectl seam these tests actually use.
//
// Package main's helper of the same name also reinstalls configreadiness's
// capabilities, because there that seam is shared with half a dozen verbs. Copying
// it wholesale drags an unrelated Deps install across the boundary to satisfy a
// name — the third package in this campaign to need that correction.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	prev := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = prev })
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
