package render

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// Helpers the moved tests use, copied across the boundary.

func withLookPath(t *testing.T, fn func(file string) (string, error)) {
	t.Helper()
	// Stub the ONE seam: execLookPath delegates here, so swapping this covers both.
	orig := kubectlprobe.LookPathFn
	kubectlprobe.LookPathFn = fn
	t.Cleanup(func() { kubectlprobe.LookPathFn = orig })
}

// writeCluster seeds <dir>/cluster/<name>.tfvars files for the discovery tests.
func writeCluster(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "cluster"), 0o755); err != nil {
		t.Fatalf("mkdir cluster: %v", err)
	}
	for name, body := range files {
		mustWrite(t, filepath.Join(dir, "cluster", name), body)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// chdirTempDir moves the test into a fresh temp dir (the commands resolve tfvars
// relative to the workflow's checkout root).
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

func boolPtrLocal(b bool) *bool { return &b }
