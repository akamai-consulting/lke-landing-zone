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

func TestRenderManifest_AndDriftCheck(t *testing.T) {
	root := t.TempDir()
	aplDir := filepath.Join(root, "apl-values")
	lz, err := clusterspec.Decode([]byte(`
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: i }
spec:
  instance: { upstreamOrg: o, repo: o/i, forge: github, templateVersion: main }
  environments:
    prod:
      cluster:
        clusterLabel: prod
        region: us-ord
        k8sVersion: v1.33.6+lke7
        nodePool: { type: t, count: 3 }
        bootstrap: { name: platform-prod }
        objectStorage: { cluster: us-ord-1 }
      components:
        harbor: { enabled: false }
`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The shared values.yaml base lets renderManifest exercise the apl-core backend
	// too — including the spec-owned identity write.
	if err := os.MkdirAll(filepath.Join(aplDir, "_shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aplDir, "_shared", "values.yaml"), []byte(
		"cluster:\n  name: ${cluster_name}\n  domainSuffix: ${cluster_domain}\napps:\n  harbor: { enabled: true }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prod, _ := lz.Env("prod")
	if err := renderManifest("prod", prod, lz.ValuesIdentity("prod"), aplDir, "", false); err != nil {
		t.Fatalf("renderManifest: %v", err)
	}

	kust, err := os.ReadFile(filepath.Join(aplDir, "prod", "manifest", "kustomization.yaml"))
	if err != nil {
		t.Fatalf("read kustomization: %v", err)
	}
	// Thin overlay over the shared base; harbor disabled → its component dir dropped.
	if !strings.Contains(string(kust), "../../_shared/manifest") || strings.Contains(string(kust), "../../components/harbor") {
		t.Errorf("kustomization wrong (thin overlay, harbor dropped):\n%s", kust)
	}

	// The apl-core backend rendered the spec identity into values.yaml (the
	// templatefile hop is gone) and flipped harbor off.
	vals, err := os.ReadFile(filepath.Join(aplDir, "prod", "values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	if !strings.Contains(string(vals), "name: platform-prod") || strings.Contains(string(vals), "${cluster_name}") {
		t.Errorf("values.yaml identity not rendered from spec:\n%s", vals)
	}
	if !strings.Contains(string(vals), "enabled: false") {
		t.Errorf("values.yaml harbor not flipped off:\n%s", vals)
	}

	// Freshly rendered → no drift.
	if err := checkManifestDrift(lz, aplDir, []string{"prod"}); err != nil {
		t.Fatalf("expected no drift after render; got %v", err)
	}
	// Tamper → drift detected.
	if err := os.WriteFile(filepath.Join(aplDir, "prod", "manifest", "kustomization.yaml"), []byte("hand-edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkManifestDrift(lz, aplDir, []string{"prod"}); err == nil {
		t.Error("expected drift error after tampering with the committed kustomization")
	}
}

func TestRenderNetworks(t *testing.T) {
	tfDir := filepath.Join(t.TempDir(), "terraform-iac-bootstrap")
	if err := os.MkdirAll(filepath.Join(tfDir, "vpc"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(tfDir, "vpc", tplTfvars), "vpc_label = \"x\"\nregion = \"x\"\n")

	lz, err := clusterspec.Decode([]byte(`
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: i }
spec:
  instance: { upstreamOrg: o, repo: o/i, forge: github, templateVersion: main }
  networks:
    ord-shared: { region: us-ord }
    sea-shared: { region: us-sea }
`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := renderNetworks(lz, tfDir, "", false); err != nil {
		t.Fatalf("renderNetworks: %v", err)
	}
	for name, region := range map[string]string{"ord-shared": "us-ord", "sea-shared": "us-sea"} {
		b, err := os.ReadFile(filepath.Join(tfDir, "vpc", name+".tfvars"))
		if err != nil {
			t.Fatalf("read vpc/%s.tfvars: %v", name, err)
		}
		for _, want := range []string{`vpc_label = "` + name + `"`, `region = "` + region + `"`} {
			if !strings.Contains(string(b), want) {
				t.Errorf("vpc/%s.tfvars missing %q:\n%s", name, want, b)
			}
		}
	}

	// No networks declared → nothing written (no vpc root needed).
	lz2, _ := clusterspec.Decode([]byte("apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata: { name: i }\nspec:\n  instance: { upstreamOrg: o, repo: o/i, forge: github, templateVersion: main }\n"))
	if err := renderNetworks(lz2, tfDir, "", false); err != nil {
		t.Fatalf("renderNetworks (empty): %v", err)
	}
}

// listDeployments must also discover the split layout (landingzone.yaml +
// environments/*.yaml), unioned with committed tfvars.
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
	if err := os.MkdirAll(filepath.Join(root, clusterspec.EnvironmentsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, clusterspec.EnvironmentsDir, "prod.yaml"), `
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
