package clusteraccess

// acl_state_test.go — moved from package main's coverage_tier2_test.go, another
// grab-bag. removeRunnerACLState is the ACL's own state file, so its test belongs
// beside it.

import (
	"os"
	"testing"
)

func TestRemoveRunnerACLState(t *testing.T) {
	t.Setenv("RUNNER_TEMP", t.TempDir())

	// Absent file is not an error.
	if err := removeRunnerACLState("ord"); err != nil {
		t.Errorf("remove absent = %v, want nil", err)
	}
	// Present file is removed.
	path := runnerACLStatePath("ord")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeRunnerACLState("ord"); err != nil {
		t.Errorf("remove present = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("state file still present after remove")
	}
}
