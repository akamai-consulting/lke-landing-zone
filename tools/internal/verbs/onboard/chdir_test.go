package onboard

// chdir_test.go — local copy for the doctor gate test that came from package
// main; several packages carry the same eight lines.

import (
	"os"
	"testing"
)

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
