package assertsecrets

// A COPY of package main's rawOpenBaoValues fixture — it reads the rendered OpenBao
// values so the audit lane's tests can assert against the real mount paths.

import (
	"os"
	"path/filepath"
	"testing"
)

// rawOpenBaoValues returns kubernetes-charts/llz-openbao-platform/values.yaml as
// text, for the assertions that inspect the embedded HCL and volume mounts rather
// than the YAML structure.
func rawOpenBaoValues(t *testing.T) string {
	t.Helper()
	return readForTLSTest(t, repoRootForTLSTest(t), filepath.ToSlash(openbaoChartValues))
}

// Path to the rendered OpenBao chart values, repo-relative from this package.
// openbaoChartValues is the path to the llz-openbao-platform values, relative to
// the repo root.
const openbaoChartValues = "kubernetes-charts/llz-openbao-platform/values.yaml"

func readForTLSTest(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// A COPY. It WALKS UP to the repo root rather than using a relative path, which
// is why it survives the move unchanged — the one fixture in this batch that did.
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
