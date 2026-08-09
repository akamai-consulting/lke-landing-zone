package argodiag

// helpers_test.go — fixtures the moved tests need, and the seam swap that makes
// them mean anything.
//
// THIS PACKAGE TOUCHES internal/kubectlprobe, WHICH HAS TWO STANDING TRAPS, and
// both are paid here rather than discovered later:
//
//  1. kubectlprobe.Delay is a real sleep between probe retries. A package that
//     leaves it set runs its suite at wall-clock speed — internal/converge went
//     from 4s to 568s and started tripping CI's 300s timeout before TestMain
//     zeroed it.
//  2. withKubectl in package main stubs `execOutput`, which this package no
//     longer calls: the extraction rewired it to kubectlprobe.Exec. Copying that
//     helper verbatim would leave every probe reaching a REAL cluster while the
//     test looked stubbed — the double-seam failure this campaign has hit before.
//     So this copy swaps kubectlprobe.Exec, which is the seam that is actually
//     live here.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

func TestMain(m *testing.M) {
	kubectlprobe.Delay = 0
	os.Exit(m.Run())
}

// withKubectl answers kubectl invocations from a handler keyed on the joined
// args; anything else errors, which the probe helpers read as "empty".
func withKubectl(t *testing.T, h func(args string) ([]byte, error)) {
	t.Helper()
	prev := kubectlprobe.Exec
	kubectlprobe.Exec = func(name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			return nil, fmt.Errorf("unexpected command %q", name)
		}
		return h(strings.Join(args, " "))
	}
	t.Cleanup(func() { kubectlprobe.Exec = prev })
}

// items wraps item JSON blobs into a kubectl list response.
func items(blobs ...string) []byte {
	return []byte(`{"items":[` + strings.Join(blobs, ",") + `]}`)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// withExecOutput swaps the ONE live shell-out seam this package has.
//
// Package main's helper of the same name also reinstalls configreadiness's
// capabilities, because in main that seam is shared with half a dozen other
// verbs. None of that applies here — the extraction left this package with a
// single dependency on kubectlprobe.Exec — and copying the original wholesale
// would have dragged an unrelated Deps install across the boundary to satisfy a
// name.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	prev := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = prev })
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
