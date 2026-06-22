package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

func TestApplyAssigns(t *testing.T) {
	base := "region = \"PLACEHOLDER\"\n# obj_key_rotation_days = 120\n"
	out := applyAssigns(base, []clusterspec.Assign{
		{Key: "region", Val: `"us-ord"`},          // replaces existing line
		{Key: "obj_key_rotation_days", Val: "90"}, // commented in example → appended
	})
	if !strings.Contains(out, `region = "us-ord"`) {
		t.Errorf("region not replaced:\n%s", out)
	}
	if strings.Contains(out, `"PLACEHOLDER"`) {
		t.Errorf("placeholder survived:\n%s", out)
	}
	if !strings.Contains(out, "obj_key_rotation_days = 90") {
		t.Errorf("commented key not appended:\n%s", out)
	}
}

// stageRoots writes a minimal terraform.tfvars.example under each TF root.
func stageRoots(t *testing.T, tfDir string) {
	t.Helper()
	examples := map[string]string{
		"cluster":           "cluster_label = \"x\"\nregion = \"x\"\nk8s_version = \"x\"\nnode_type = \"x\"\nnode_count = 1\ntags = []\ncontrol_plane_high_availability = false\ncontrol_plane_audit_logs_enabled = false\nha_role = \"standalone\"\n",
		"cluster-bootstrap": "deployment = \"your-env\"\napl_values_env = \"your-env\"\ncluster_name = \"platform-your-env\"\ncluster_domain = \"your-env.internal\"\n",
		"object-storage":    "region_suffix = \"your-env\"\nobj_cluster = \"us-ord-1\"\n",
	}
	for root, body := range examples {
		if err := os.MkdirAll(filepath.Join(tfDir, root), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", root, err)
		}
		mustWrite(t, filepath.Join(tfDir, root, tplTfvars), body)
	}
}

const renderSpec = `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: i }
spec:
  instance: { upstreamOrg: o, repo: o/i, forge: github, templateVersion: main }
  environments:
    prod:
      cluster:
        clusterLabel: platform-prod
        region: us-ord
        k8sVersion: v1.33.6+lke7
        nodePool: { type: g8-dedicated-8-4, count: 5 }
        bootstrap: { name: platform-prod, domainSuffix: prod.example.com }
        objectStorage: { cluster: us-ord-7 }
`

func TestRenderEnvTfvars(t *testing.T) {
	tfDir := filepath.Join(t.TempDir(), "terraform-iac-bootstrap")
	stageRoots(t, tfDir)

	lz, err := clusterspec.Decode([]byte(renderSpec))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	c := lz.Spec.Environments["prod"].Cluster
	if err := renderEnvTfvars("prod", c, tfDir, "", false); err != nil {
		t.Fatalf("renderEnvTfvars: %v", err)
	}

	read := func(root string) string {
		b, err := os.ReadFile(filepath.Join(tfDir, root, "prod.tfvars"))
		if err != nil {
			t.Fatalf("read %s/prod.tfvars: %v", root, err)
		}
		return string(b)
	}
	cluster := read("cluster")
	for _, want := range []string{`cluster_label = "platform-prod"`, `region = "us-ord"`, `node_count = 5`} {
		if !strings.Contains(cluster, want) {
			t.Errorf("cluster/prod.tfvars missing %q:\n%s", want, cluster)
		}
	}
	boot := read("cluster-bootstrap")
	for _, want := range []string{`deployment = "prod"`, `apl_values_env = "prod"`, `cluster_domain = "prod.example.com"`} {
		if !strings.Contains(boot, want) {
			t.Errorf("cluster-bootstrap/prod.tfvars missing %q:\n%s", want, boot)
		}
	}
	if strings.Contains(boot, "your-env") {
		t.Errorf("cluster-bootstrap still has your-env sentinel:\n%s", boot)
	}
	obj := read("object-storage")
	for _, want := range []string{`region_suffix = "prod"`, `obj_cluster = "us-ord-7"`} {
		if !strings.Contains(obj, want) {
			t.Errorf("object-storage/prod.tfvars missing %q:\n%s", want, obj)
		}
	}
}

// listDeployments must also discover the split layout (landingzone.yaml +
// clusters/*.yaml), unioned with committed tfvars.
func TestListDeployments_SplitSpecUnion(t *testing.T) {
	root := t.TempDir()
	tfDir := filepath.Join(root, "terraform-iac-bootstrap")
	writeCluster(t, tfDir, map[string]string{
		"legacy.tfvars": "region = \"us-sea\"\n",
	})
	mustWrite(t, filepath.Join(root, clusterspec.LandingZoneFile), `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: i }
spec:
  instance: { upstreamOrg: o, repo: o/i, forge: github, templateVersion: main }
`)
	if err := os.MkdirAll(filepath.Join(root, clusterspec.ClustersDir), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, clusterspec.ClustersDir, "prod.yaml"), `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { name: prod }
spec:
  cluster:
    clusterLabel: platform-prod
    region: us-ord
    k8sVersion: v1.33.6+lke7
    nodePool: { type: g8-dedicated-8-4, count: 5 }
    bootstrap: { name: platform-prod }
    objectStorage: { cluster: us-ord-1 }
`)

	got, err := listDeployments(tfDir)
	if err != nil {
		t.Fatalf("listDeployments: %v", err)
	}
	if want := []string{"legacy", "prod"}; !reflect.DeepEqual(got, want) {
		t.Errorf("split union = %v, want %v", got, want)
	}
}

// listDeployments must union committed tfvars with spec.environments so an
// instance can migrate env-by-env.
func TestListDeployments_SpecUnion(t *testing.T) {
	root := t.TempDir()
	tfDir := filepath.Join(root, "terraform-iac-bootstrap")
	writeCluster(t, tfDir, map[string]string{
		"legacy.tfvars": "region = \"us-sea\"\n",
	})
	mustWrite(t, filepath.Join(root, clusterspec.DefaultFile), renderSpec) // declares env "prod"

	got, err := listDeployments(tfDir)
	if err != nil {
		t.Fatalf("listDeployments: %v", err)
	}
	want := []string{"legacy", "prod"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listDeployments union = %v, want %v", got, want)
	}
}
