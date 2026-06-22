package clusterspec

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func bptr(b bool) *bool { return &b }

// writeInstance lays out a set of relative path → content files under a temp dir
// and returns the dir (the instance root passed to LoadInstance/LoadSplit).
func writeInstance(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return root
}

const splitLandingZone = `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: my-instance }
spec:
  instance: { upstreamOrg: akamai-consulting, repo: my-org/my-instance, forge: github, templateVersion: v0.4.0 }
  defaults:
    cluster:
      k8sVersion: v1.33.6+lke7
      nodePool: { type: g8-dedicated-8-4, count: 5 }
      controlPlane: { highAvailability: true, auditLogsEnabled: true }
`

const splitProd = `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { name: prod }
spec:
  cluster:
    clusterLabel: platform-prod
    region: us-ord
    bootstrap: { name: platform-prod }
    objectStorage: { cluster: us-ord-1 }
`

const splitStaging = `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { name: staging }
spec:
  cluster:
    clusterLabel: platform-staging
    region: us-sea
    nodePool: { count: 3 }
    bootstrap: { name: platform-staging }
    objectStorage: { cluster: us-sea-1 }
  recipes:
    harbor: { enabled: false }
`

func splitFiles() map[string]string {
	return map[string]string{
		"landingzone.yaml":      splitLandingZone,
		"clusters/prod.yaml":    splitProd,
		"clusters/staging.yaml": splitStaging,
	}
}

func TestLoadSplit_InheritanceAndValidate(t *testing.T) {
	root := writeInstance(t, splitFiles())
	lz, err := LoadInstance(root)
	if err != nil {
		t.Fatalf("LoadInstance: %v", err)
	}
	if errs := lz.Validate(); len(errs) != 0 {
		t.Fatalf("expected valid assembled spec, got: %v", errs)
	}
	if got := lz.EnvNames(); !reflect.DeepEqual(got, []string{"prod", "staging"}) {
		t.Fatalf("env names = %v, want [prod staging]", got)
	}

	prod := lz.Spec.Environments["prod"].Cluster
	if prod.K8sVersion != "v1.33.6+lke7" {
		t.Errorf("prod k8sVersion = %q, want inherited v1.33.6+lke7", prod.K8sVersion)
	}
	if prod.NodePool.Type != "g8-dedicated-8-4" || prod.NodePool.Count != 5 {
		t.Errorf("prod nodePool = %+v, want inherited type/count", prod.NodePool)
	}
	if prod.ControlPlane.HighAvailability == nil || !*prod.ControlPlane.HighAvailability {
		t.Errorf("prod controlPlane.highAvailability should inherit true")
	}
	if got := lz.Spec.Environments["prod"].Cluster.Bootstrap.DomainSuffix; got != "prod.internal" {
		t.Errorf("prod domainSuffix default = %q, want prod.internal", got)
	}

	// staging overrides count, inherits type.
	st := lz.Spec.Environments["staging"].Cluster
	if st.NodePool.Count != 3 {
		t.Errorf("staging count = %d, want override 3", st.NodePool.Count)
	}
	if st.NodePool.Type != "g8-dedicated-8-4" {
		t.Errorf("staging type = %q, want inherited g8-dedicated-8-4", st.NodePool.Type)
	}
	if lz.Spec.Environments["staging"].Recipes["harbor"].Enabled {
		t.Error("staging harbor recipe should be disabled by override")
	}
}

func TestLoadInstance_LayoutDetection(t *testing.T) {
	// split only
	if _, err := LoadInstance(writeInstance(t, splitFiles())); err != nil {
		t.Errorf("split layout: %v", err)
	}
	// single only
	single := writeInstance(t, map[string]string{"llz.yaml": validSpec})
	if _, err := LoadInstance(single); err != nil {
		t.Errorf("single layout: %v", err)
	}
	// neither → ErrNoSpec
	if _, err := LoadInstance(t.TempDir()); !errors.Is(err, ErrNoSpec) {
		t.Errorf("empty dir err = %v, want ErrNoSpec", err)
	}
	// both → ambiguity error (not ErrNoSpec)
	both := writeInstance(t, map[string]string{"llz.yaml": validSpec, "landingzone.yaml": splitLandingZone})
	if _, err := LoadInstance(both); err == nil || errors.Is(err, ErrNoSpec) {
		t.Errorf("both layouts err = %v, want an ambiguity error", err)
	}
}

func TestLoadSplit_DuplicateEnv(t *testing.T) {
	files := splitFiles()
	files["clusters/prod-dup.yaml"] = splitProd // same metadata.name: prod
	if _, err := LoadInstance(writeInstance(t, files)); err == nil {
		t.Fatal("expected duplicate-env error")
	}
}

func TestLoadClusterDefinition_Structural(t *testing.T) {
	bad := map[string]string{
		"bad kind":       "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: Nope\nmetadata: { name: x }\nspec: {}\n",
		"missing name":   "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: ClusterDefinition\nmetadata: {}\nspec: {}\n",
		"bad apiVersion": "apiVersion: v0\nkind: ClusterDefinition\nmetadata: { name: x }\nspec: {}\n",
	}
	for name, body := range bad {
		root := writeInstance(t, map[string]string{"landingzone.yaml": splitLandingZone, "clusters/x.yaml": body})
		if _, err := LoadInstance(root); err == nil {
			t.Errorf("%s: expected a structural load error", name)
		}
	}
}

// TestSplitEqualsSingle proves the two layouts are schema-compatible: a split
// instance assembles to the same environments as an equivalent single-file one.
func TestSplitEqualsSingle(t *testing.T) {
	split, err := LoadInstance(writeInstance(t, splitFiles()))
	if err != nil {
		t.Fatalf("split load: %v", err)
	}

	const equivalent = `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: my-instance }
spec:
  instance: { upstreamOrg: akamai-consulting, repo: my-org/my-instance, forge: github, templateVersion: v0.4.0 }
  defaults:
    cluster:
      k8sVersion: v1.33.6+lke7
      nodePool: { type: g8-dedicated-8-4, count: 5 }
      controlPlane: { highAvailability: true, auditLogsEnabled: true }
  environments:
    prod:
      cluster:
        clusterLabel: platform-prod
        region: us-ord
        bootstrap: { name: platform-prod }
        objectStorage: { cluster: us-ord-1 }
    staging:
      cluster:
        clusterLabel: platform-staging
        region: us-sea
        nodePool: { count: 3 }
        bootstrap: { name: platform-staging }
        objectStorage: { cluster: us-sea-1 }
      recipes:
        harbor: { enabled: false }
`
	single := mustDecode(t, equivalent)
	if !reflect.DeepEqual(split.Spec.Environments, single.Spec.Environments) {
		t.Errorf("split vs single environments differ:\n split=%+v\nsingle=%+v",
			split.Spec.Environments, single.Spec.Environments)
	}
}

func TestMergeCluster_Precedence(t *testing.T) {
	base := Cluster{
		Region:              "us-ord",
		NodePool:            NodePool{Type: "big", Count: 5, AutoscalerEnabled: bptr(true)},
		APIServerAllowCIDRs: AllowCIDRs{IPv4: []string{"1.2.3.0/24"}},
	}
	// Override: explicit empty list and autoscaler:false must WIN over the
	// non-empty/true defaults (a deliberate zero is not "unset").
	over := Cluster{
		NodePool:            NodePool{AutoscalerEnabled: bptr(false)},
		APIServerAllowCIDRs: AllowCIDRs{IPv4: []string{}},
	}
	got := mergeCluster(base, over)
	if got.Region != "us-ord" {
		t.Errorf("region = %q, want inherited us-ord", got.Region)
	}
	if got.NodePool.Type != "big" || got.NodePool.Count != 5 {
		t.Errorf("nodePool type/count should inherit, got %+v", got.NodePool)
	}
	if got.NodePool.AutoscalerEnabled == nil || *got.NodePool.AutoscalerEnabled {
		t.Errorf("autoscaler should be overridden to false")
	}
	if got.APIServerAllowCIDRs.IPv4 == nil || len(got.APIServerAllowCIDRs.IPv4) != 0 {
		t.Errorf("explicit empty IPv4 list should win, got %v", got.APIServerAllowCIDRs.IPv4)
	}

	// nil override inherits the base slice.
	if g := mergeCluster(base, Cluster{}).APIServerAllowCIDRs.IPv4; !reflect.DeepEqual(g, []string{"1.2.3.0/24"}) {
		t.Errorf("nil override should inherit base IPv4, got %v", g)
	}
}
