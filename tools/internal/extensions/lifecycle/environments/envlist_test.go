package environments

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	topo "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envtopology"
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
		"terraform.tfvars":         "# local override, not a topo.Deployment\n",
		"Bad_Name.tfvars":          "# invalid basename, must be skipped\n",
	})

	got, err := topo.ListDeployments(dir)
	if err != nil {
		t.Fatalf("topo.ListDeployments: %v", err)
	}
	// Sorted; example/terraform/invalid excluded.
	want := []string{"lab", "primary", "secondary"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("topo.ListDeployments = %v, want %v", got, want)
	}
}

func TestListDeploymentsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cluster"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got, err := topo.ListDeployments(dir)
	if err != nil {
		t.Fatalf("topo.ListDeployments: %v", err)
	}
	// Must be a non-nil empty slice so `--json` marshals to [] not null.
	if got == nil {
		t.Fatal("topo.ListDeployments returned nil; want non-nil empty slice (JSON [] not null)")
	}
	if len(got) != 0 {
		t.Errorf("topo.ListDeployments = %v, want empty", got)
	}
}

func TestListDeploymentsNoClusterDir(t *testing.T) {
	// A tfDir with no cluster/ at all (e.g. a fresh checkout) → empty, no error.
	got, err := topo.ListDeployments(t.TempDir())
	if err != nil {
		t.Fatalf("topo.ListDeployments: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("topo.ListDeployments = %v, want empty", got)
	}
}

func TestWriteHAResolution(t *testing.T) {
	deps := []topo.Deployment{
		{Name: "east", HARole: topo.RoleActive, HAGroup: "g1"},
		{Name: "west", HARole: topo.RoleStandby, HAGroup: "g1"},
		{Name: "solo", HARole: "standalone"}, // roleStandalone stays unexported in shared/envtopology
	}
	for _, tc := range []struct {
		name, wantRole, wantPeer string
	}{
		{"east", "active", "west"},
		{"west", "standby", "east"},
		{"solo", "standalone", ""}, // standalone → peer empty, not an error
	} {
		out := filepath.Join(t.TempDir(), "output")
		t.Setenv("GITHUB_OUTPUT", out)
		if err := writeHAResolution(deps, tc.name); err != nil {
			t.Fatalf("writeHAResolution(%s): %v", tc.name, err)
		}
		b, _ := os.ReadFile(out)
		got := string(b)
		if !strings.Contains(got, "role="+tc.wantRole) || !strings.Contains(got, "peer="+tc.wantPeer+"\n") {
			t.Errorf("%s → GITHUB_OUTPUT %q, want role=%s peer=%q", tc.name, got, tc.wantRole, tc.wantPeer)
		}
	}
	if err := writeHAResolution(deps, "nope"); err == nil {
		t.Error("unknown Deployment must error")
	}
}
