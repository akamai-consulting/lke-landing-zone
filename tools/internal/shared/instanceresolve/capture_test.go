package instanceresolve

// capture_test.go — local stderr capture for the account-check test that came
// from package main. Several packages carry their own copy; a testutil package
// existing only to be imported by tests costs more than the duplication.

import (
	"io"
	"os"
	"testing"
)

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
