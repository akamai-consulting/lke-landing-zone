package openbao

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Helpers the moved tests use, copied across the new package boundary.

// captureStderr mirrors captureStdout for the os.Stderr path (the remediation /
// warning printers write there).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = orig
	var b strings.Builder
	if _, err := io.Copy(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func readForTLSTest(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// repoRootForTLSTest walks up from the package dir to the repo root (the dir
// holding platform-apl/), so the test is independent of where `go test` runs.
func repoRootForTLSTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "platform-apl")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("repo root (platform-apl/) not found — running outside a source checkout")
	return ""
}
