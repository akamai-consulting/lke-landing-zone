package assertsecrets

// cobra_capture_test.go — stdout/stderr capture for the tests that came with the
// moved commands. A local copy rather than a shared testutil package: it is
// twenty lines with no production caller, and hoisting it would put a non-test
// package in the tree whose only job is to be imported by tests.

import (
	"io"
	"os"
	"testing"
)

func captureStdoutStderr(t *testing.T, fn func()) (stdout, stderr string) {
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
