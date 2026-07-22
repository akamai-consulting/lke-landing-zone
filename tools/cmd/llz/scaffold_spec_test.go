package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// TestEnvAddSpecAuthoring covers the spec-first half of `llz env add`:
// ensureLandingZone + writeEnvDefinition must produce a spec that LoadInstance +
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
	write("terraform-iac-bootstrap/cluster/terraform.tfvars.example",
		"cluster_label = \"x\"\nk8s_version = \"v1.33.6+lke7\"\nnode_type  = \"g8-dedicated-8-4\"\nnode_count = 5\n")

	// First env: creates landingzone.yaml from the answers + seeded defaults.
	name, created, err := ensureLandingZone(dir)
	if err != nil || !created {
		t.Fatalf("ensureLandingZone created=%v err=%v", created, err)
	}
	if name != "platform-support" {
		t.Fatalf("instance name = %q, want platform-support", name)
	}
	// Idempotent: a second call leaves it as-is.
	if _, created2, _ := ensureLandingZone(dir); created2 {
		t.Error("ensureLandingZone recreated an existing landingzone.yaml")
	}

	// Author one env from the must-set flags only; the rest inherits defaults.
	envFile := filepath.Join(dir, "environments", "lab.yaml")
	if err := writeEnvDefinition(envFile, "lab",
		envAddOpts{region: "us-sea", objCluster: "us-sea-1", nodeCount: "3"},
		name, "lab.internal"); err != nil {
		t.Fatalf("writeEnvDefinition: %v", err)
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
	if c.K8sVersion != "v1.33.6+lke7" {
		t.Errorf("k8sVersion = %q, want inherited v1.33.6+lke7", c.K8sVersion)
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
		if got := shortRepoName(in); got != want {
			t.Errorf("shortRepoName(%q) = %q, want %q", in, got, want)
		}
	}
}
