package firewall

import (
	"os"
	"testing"
)

// Helpers the moved tests use, copied across the new package boundary.

// chdir cds into dir for the duration of the test, restoring the cwd after.
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
