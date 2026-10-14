package deps

// helpers_test.go — the two fixture helpers managedfresh_test.go uses.
//
// COPIED FROM internal/cli RATHER THAN SHARED, and that is the ordinary Go answer
// rather than a compromise: test helpers are not part of a package's surface, and
// exporting them to share fourteen lines would put fixture plumbing in the import
// graph of everything that uses this package. `chdir` is t.Chdir's older spelling,
// kept because the tests that moved here were written against it.

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
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
