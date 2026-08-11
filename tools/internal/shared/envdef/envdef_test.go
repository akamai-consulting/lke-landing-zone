package envdef

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// TestEnvAddSpecAuthoring covers the spec-first half of `llz env add`:
// EnsureLandingZone + WriteEnvDefinition must produce a spec that LoadInstance +
// Validate accept, with the env inheriting the seeded spec.defaults.
func TestEnvAddSpecAuthoring(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".copier-answers.yml", "upstream_org: akamai-consulting\ninstance_repo: my-org/platform-support\nllz_version: v0.4.0\nopenbao_team: ops\n")
	// NOTE: this local file is NOT what supplies the defaults. tfvarsExampleValue
	// reads the EMBEDDED tfroots copy (an instance ships no .example any more), so
	// the values asserted below come from
	// tools/internal/shared/tfroots/roots/cluster/terraform.tfvars.example. It is
	// written anyway because `llz env add` runs against a tree that has one, and
	// leaving it out would test a layout no instance has.
	//
	// It also used to make this test LIE: it wrote k8s_version = "v1.34.6+lke2" and
	// the assertion said "want inherited v1.34.6+lke2", which passed only because
	// the embedded copy carried the same string. Bumping the embedded default
	// alone broke it — proving the fixture was never the source. The assertions
	// now name the embedded example as the authority.
	write("terraform-iac-bootstrap/cluster/terraform.tfvars.example",
		"cluster_label = \"x\"\nk8s_version = \"v0.0.0+ignored\"\nnode_type  = \"g8-dedicated-8-4\"\nnode_count = 5\n")

	// First env: creates landingzone.yaml from the answers + seeded defaults.
	name, created, err := EnsureLandingZone(dir)
	if err != nil || !created {
		t.Fatalf("EnsureLandingZone created=%v err=%v", created, err)
	}
	if name != "platform-support" {
		t.Fatalf("instance name = %q, want platform-support", name)
	}
	// Idempotent: a second call leaves it as-is.
	if _, created2, _ := EnsureLandingZone(dir); created2 {
		t.Error("EnsureLandingZone recreated an existing landingzone.yaml")
	}

	// Author one env from the must-set flags only; the rest inherits defaults.
	envFile := filepath.Join(dir, "environments", "lab.yaml")
	if err := WriteEnvDefinition(envFile, "lab",
		Opts{Region: "us-sea", ObjCluster: "us-sea-1", NodeCount: "3"},
		name); err != nil {
		t.Fatalf("WriteEnvDefinition: %v", err)
	}

	// The assembled spec must load + validate.
	lz, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatalf("LoadInstance: %v", err)
	}
	if errs := lz.Validate(); len(errs) != 0 {
		t.Fatalf("authored spec should validate, got: %v", errs)
	}
	e, ok := lz.Env("lab")
	if !ok {
		t.Fatal("env lab missing")
	}
	c := e.Cluster
	if c.Region != "us-sea" || c.ObjectStorage.Cluster != "us-sea-1" {
		t.Errorf("flags not applied: region=%q obj=%q", c.Region, c.ObjectStorage.Cluster)
	}
	if c.ClusterLabel != "platform-support-lab" || c.Bootstrap.Name != "platform-support-lab" {
		t.Errorf("identity not derived: label=%q name=%q", c.ClusterLabel, c.Bootstrap.Name)
	}
	// From the EMBEDDED tfroots example, not the local fixture above.
	wantK8s := embeddedExampleK8sVersion(t)
	if c.K8sVersion != wantK8s {
		t.Errorf("k8sVersion = %q, want %q from the embedded tfroots terraform.tfvars.example", c.K8sVersion, wantK8s)
	}
	if c.NodePool.Type != "g8-dedicated-8-4" || c.NodePool.Count != 3 {
		t.Errorf("nodePool = %+v, want type inherited + count override 3", c.NodePool)
	}
	if c.HA.Role != "standalone" {
		t.Errorf("ha.role = %q, want standalone default", c.HA.Role)
	}
	// The copier openbao_team answer becomes spec.teams[0] (secret/<name>), so the
	// operator's chosen team — not the hardcoded platform default — is authored.
	if len(lz.Spec.Teams) != 1 || lz.Spec.Teams[0].Name != "ops" ||
		lz.Spec.Teams[0].OpenbaoSubtree != "secret/ops" {
		t.Errorf("spec.teams from copier answer = %+v, want ops/secret/ops", lz.Spec.Teams)
	}
}

func TestShortRepoName(t *testing.T) {
	for in, want := range map[string]string{"o/r": "r", "a/b/c": "c", "plain": "plain", "": ""} {
		if got := ShortRepoName(in); got != want {
			t.Errorf("ShortRepoName(%q) = %q, want %q", in, got, want)
		}
	}
}

// embeddedExampleK8sVersion reads the k8s_version the embedded cluster root's
// terraform.tfvars.example carries — the single place the default lives, so this
// test tracks a bump instead of pinning a copy of one.
func embeddedExampleK8sVersion(t *testing.T) string {
	t.Helper()
	v := tfvarsExampleValue("cluster", "k8s_version")
	if v == "" {
		t.Fatal("the embedded cluster terraform.tfvars.example has no k8s_version — " +
			"every default below would silently fall back")
	}
	return v
}
