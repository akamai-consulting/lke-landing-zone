package clusterspec_test

// EXTERNAL TEST PACKAGE, like hagroup_test.go beside it and for the same reason:
// this reaches internal/envtopology, which imports clusterspec. An in-package
// test closes that loop and Go rejects it.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/render"
)

// writeSpecInstance lays a minimal spec-driven instance into the current dir: a
// landingzone.yaml + one environments/<env>.yaml per (name, body) pair. Only the
// spec YAMLs are needed — clusterspec.Detected/readTopology read those, not the tfvars.
func writeSpecInstance(t *testing.T, envs map[string]string) {
	t.Helper()
	writeFileMkdir(t, "landingzone.yaml", `apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: inst }
spec:
  instance: { upstreamOrg: o, repo: o/inst, forge: github, templateVersion: main }
  defaults:
    cluster:
      k8sVersion: v1.33.6+lke7
      nodePool: { type: g8-dedicated-8-4, count: 5 }
`)
	for name, body := range envs {
		writeFileMkdir(t, filepath.Join("environments", name+".yaml"), body)
	}
}

func clusterDef(name, extra string) string {
	return `apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { name: ` + name + ` }
spec:
  cluster:
    clusterLabel: inst-` + name + `
    region: us-ord
    bootstrap: { name: inst-` + name + ` }
    objectStorage: { cluster: us-ord-1 }
` + extra
}

// #5: render --diff reports new files for an un-rendered env, and a no-op once the
// committed apl-values match.
// #5: render --diff reports new files for an un-rendered env, and a no-op once the
// committed apl-values match.
func TestRenderDiff(t *testing.T) {
	chdirTempDir(t)
	writeSpecInstance(t, map[string]string{"lab": clusterDef("lab", "")})
	// Minimal tfvars examples + an apl-values example so render has all its inputs.
	writeFileMkdir(t, "terraform-iac-bootstrap/cluster/terraform.tfvars.example", "cluster_label = \"x\"\n")
	writeFileMkdir(t, "terraform-iac-bootstrap/cluster-bootstrap/terraform.tfvars.example", "cluster_name = \"x\"\n")
	writeFileMkdir(t, "terraform-iac-bootstrap/object-storage/terraform.tfvars.example", "obj_cluster = \"x\"\n")
	writeFileMkdir(t, filepath.Join("apl-values", "values.yaml"), "apps:\n  harbor: { enabled: true }\n")

	lz, present, err := clusterspec.Detected()
	if !present || err != nil {
		t.Fatalf("clusterspec.Detected present=%v err=%v", present, err)
	}
	var rerr error
	out := captureStdout(t, func() {
		rerr = render.RunDiff(lz, []string{"lab"}, "terraform-iac-bootstrap", "apl-values", false)
	})
	if rerr != nil {
		t.Fatalf("render.RunDiff: %v", rerr)
	}
	if !strings.Contains(out, "+ new") || !strings.Contains(out, "would change") {
		t.Errorf("diff should report new files:\n%s", out)
	}
}

// #4: the components registry view is accurate.
func TestComponentsRegistryView(t *testing.T) {
	byName := map[string]clusterspec.Component{}
	for _, c := range clusterspec.Components {
		byName[c.Name] = c
	}
	if got := clusterspec.ComponentDefault(byName["argocd"]); got != "on (required)" {
		t.Errorf("argocd default = %q", got)
	}
	if got := clusterspec.ComponentDefault(byName["harbor"]); got != "on" {
		t.Errorf("harbor default = %q", got)
	}
	if b := byName["observability"].Backends(); strings.Join(b, ",") != "apl-core,llz-argo" {
		t.Errorf("observability backends = %v, want apl-core,llz-argo", b)
	}
	if k := clusterspec.ComponentKnobs("observability"); strings.Join(k, ",") != "retention,storage,replicas" {
		t.Errorf("observability knobs = %v", k)
	}
}
