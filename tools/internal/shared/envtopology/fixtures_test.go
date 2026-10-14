package envtopology

// writeCluster, copied not shared -- same rule as every other fixture in this
// tree. The HA topology MODEL moved down here and its tests came with it; the
// tfvars fixture they read is four lines and lives in both packages rather than
// dragging a `testing` import into production code.

import (
	"os"
	"path/filepath"
	"testing"
)

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
