package main

import (
	"os"
	"path/filepath"
	"reflect"
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

func TestListDeployments(t *testing.T) {
	dir := t.TempDir()
	writeCluster(t, dir, map[string]string{
		"primary.tfvars":           "region = \"us-sea\"\n",
		"secondary.tfvars":         "region = \"us-lax\"\n",
		"lab.tfvars":               "region = \"us-ord\"\n",
		"terraform.tfvars.example": "# template\n",
		"terraform.tfvars":         "# local override, not a deployment\n",
		"Bad_Name.tfvars":          "# invalid basename, must be skipped\n",
	})

	got, err := listDeployments(dir)
	if err != nil {
		t.Fatalf("listDeployments: %v", err)
	}
	// Sorted; example/terraform/invalid excluded.
	want := []string{"lab", "primary", "secondary"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listDeployments = %v, want %v", got, want)
	}
}

func TestListDeploymentsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cluster"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got, err := listDeployments(dir)
	if err != nil {
		t.Fatalf("listDeployments: %v", err)
	}
	// Must be a non-nil empty slice so `--json` marshals to [] not null.
	if got == nil {
		t.Fatal("listDeployments returned nil; want non-nil empty slice (JSON [] not null)")
	}
	if len(got) != 0 {
		t.Errorf("listDeployments = %v, want empty", got)
	}
}

func TestListDeploymentsNoClusterDir(t *testing.T) {
	// A tfDir with no cluster/ at all (e.g. a fresh checkout) → empty, no error.
	got, err := listDeployments(t.TempDir())
	if err != nil {
		t.Fatalf("listDeployments: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("listDeployments = %v, want empty", got)
	}
}
