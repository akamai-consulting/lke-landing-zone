package main

// capture_test.go — stdout/stderr capture, kept in package main after the teardown
// extraction took the copy that used to live in ci_teardown_mutation_test.go with
// it. Several unrelated mutation tests here still need it.

import (
	"os"
	"testing"
)

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
