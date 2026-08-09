package main

// capture_test.go — stdout/stderr capture, kept in package main after the teardown
// extraction took the copy that used to live in ci_teardown_mutation_test.go with
// it. Several unrelated mutation tests here still need it.

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

// errString is a string-valued error, duplicated back into package main after the
// cluster-access extraction took the copy in acl_configmap_test.go with it.
// Several unrelated tests here still build errors this way, and a test fixture
// cannot cross a package boundary.
type errString string

func (e errString) Error() string { return string(e) }

// renderReexecChild — duplicated back into package main after the cluster-access
// extraction took the copy in fetchkubeconfig_state_deadline_test.go with it.
//
// package main's TestMain needs it for the same reason clusteraccess's does: under
// `go test`, os.Executable() is THIS binary, so renderRootsFn's `<self> render
// <env> --tfvars-only` shell-out re-runs the whole suite recursively and the run
// HANGS rather than failing. Two TestMains, two guards.
func renderReexecChild() bool {
	return len(os.Args) >= 2 && os.Args[1] == "render"
}
