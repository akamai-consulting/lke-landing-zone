package clusteraccess

import (
	"io"
	"os"
	"testing"
)

// testDeps is the Deps a unit test hands the cluster-access verbs.
//
// Exec returns EMPTY OUTPUT AND NO ERROR — the shape of a command that ran and
// found nothing, which is the honest default here. Returning an error instead
// would send every case down its failure branch, and returning canned output
// would make each test assert against a fixture nobody wrote for it.
func testDeps(t *testing.T) Deps {
	t.Helper()
	return Deps{
		Exec: func(string, ...string) ([]byte, error) { return nil, nil },
	}
}

// withExec adapts the exec-level stubs these tests were written against to the
// Exec seam, and returns the Deps to hand in. Same adapter shape as chartguard's
// withGit: keeping it means the fixtures still assert on the argv the code
// actually builds, which is the part worth checking.
func withExec(t *testing.T, stub func(string, ...string) ([]byte, error)) Deps {
	t.Helper()
	d := testDeps(t)
	d.Exec = stub
	return d
}

// captureStdout / captureStderr — local copies. The diagnostics path's whole value
// is what it PRINTS when a state read fails, so those lines are the assertion.
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
	b, _ := io.ReadAll(r)
	return string(b)
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// TestMain is NOT optional in this package, and its absence is what a hung
// `go test ./...` looks like.
//
// renderRootsFn shells out to `<self> render <env> --tfvars-only`. Under `go
// test`, os.Executable() is THIS TEST BINARY — so without a guard every shell-out
// re-runs the entire package suite, which shells out again, and the run never
// terminates. It does not fail; it HANGS, which is why it costs a wall-clock
// timeout to discover rather than a red test.
//
// package main had exactly this guard already (kubectl_probe_test.go). The
// extraction moved the code that shells out WITHOUT the TestMain that protected
// it, because a guard wired into one package's TestMain is invisible to the file
// being moved. Expect this on any extraction of code that re-executes the binary.
func TestMain(m *testing.M) {
	if renderReexecChild() {
		os.Exit(0)
	}
	os.Exit(m.Run())
}
