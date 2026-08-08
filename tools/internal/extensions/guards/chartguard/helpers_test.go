package chartguard

import (
	"strings"
	"testing"
)

// testDeps is the Deps a unit test hands the chart gates.
//
// GitOutput returns EMPTY by default, which the version guard reads as "no chart
// files changed" — the no-op path. Cases that mean to exercise the guard have to
// say what changed, which is the right default here: a fixture that invented a
// diff would make every test assert against a diff nobody wrote.
func testDeps(t *testing.T) Deps {
	t.Helper()
	return Deps{
		GitOutput: func(string, ...string) (string, error) { return "", nil },
	}
}

// withGit adapts an exec-level stub — the shape these tests were already written
// against, `func(name string, args ...string) ([]byte, error)` — to the GitOutput
// seam, and returns the Deps to hand in.
//
// The adapter is the same three lines package main's gitcmd.Output is: prepend `-C
// <dir>`, trim. Keeping it here rather than rewriting every fixture means the
// tests still exercise the argv the guard actually builds, which is the part worth
// checking.
func withGit(t *testing.T, stub func(string, ...string) ([]byte, error)) Deps {
	t.Helper()
	d := testDeps(t)
	d.GitOutput = func(dir string, args ...string) (string, error) {
		out, err := stub("git", append([]string{"-C", dir}, args...)...)
		return strings.TrimSpace(string(out)), err
	}
	return d
}
