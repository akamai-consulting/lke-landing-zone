package promote

// helpers_test.go — fixtures COPIED from package main, not exported from it.
//
// mustWrite, chdir and writeCluster all live in internal/cli test files too. The
// call this branch has made consistently (fakeKubectl, pollRecorder, containsArg)
// is to copy: a fixture shared across an extraction boundary makes the extracted
// package a dependency of the CLI's own tests, which is the coupling the
// extraction exists to remove.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// testDeps returns Deps that DO THE WORK against the test's temp dir: a real
// layout, a real deployment list read off disk, and a spec lookup reporting
// "absent" so the tfvars path is exercised. A no-op set here would make every
// promotion-rank assertion run against an empty DAG.
func testDeps() Deps {
	return Deps{
		Layout:          func() (string, string, string) { return "terraform-iac-bootstrap", "platform-apl", "" },
		ListDeployments: listDeploymentsFromDisk,
		LoadSpec:        func() (*clusterspec.LandingZone, bool, error) { return nil, false, nil },
		InstanceRepo:    func() string { return "" },
	}
}

// listDeploymentsFromDisk mirrors package main's listDeployments closely enough
// for the rank tests: every cluster/*.tfvars is a deployment.
func listDeploymentsFromDisk(tfDir string) ([]string, error) {
	entries, err := filepath.Glob(filepath.Join(tfDir, "cluster", "*.tfvars"))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(filepath.Base(e), ".tfvars"))
	}
	sort.Strings(out)
	return out, nil
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

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
