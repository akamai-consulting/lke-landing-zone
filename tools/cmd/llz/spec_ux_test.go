package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// writeSpecInstance lays a minimal spec-driven instance into the current dir: a
// landingzone.yaml + one environments/<env>.yaml per (name, body) pair. Only the
// spec YAMLs are needed — loadSpec/readTopology read those, not the tfvars.
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

// #2: readTopology / promotionRanks read the SPEC, so role/peer/next stay correct
// even when no tfvars exist (a spec edit that wasn't rendered).
func TestReadTopologyFromSpec(t *testing.T) {
	chdirTempDir(t)
	writeSpecInstance(t, map[string]string{
		"east": clusterDef("east", "    ha: { role: active, group: prod }\n    promotionRank: 2\n"),
		"west": clusterDef("west", "    ha: { role: standby, group: prod }\n"),
		"lab":  clusterDef("lab", "    promotionRank: 1\n"),
	})

	deps, err := readTopology("terraform-iac-bootstrap")
	if err != nil {
		t.Fatalf("readTopology: %v", err)
	}
	d, _ := findDeployment(deps, "east")
	if d.haRole != "active" || d.haGroup != "prod" {
		t.Errorf("east from spec = %+v, want active/prod", d)
	}
	if peer, ok, err := peerOf(deps, "east"); err != nil || !ok || peer != "west" {
		t.Errorf("peerOf(east) = %q,%v,%v, want west", peer, ok, err)
	}
	if lab, _ := findDeployment(deps, "lab"); lab.haRole != "standalone" {
		t.Errorf("lab role = %q, want standalone default", lab.haRole)
	}

	ranks, err := promotionRanks("terraform-iac-bootstrap")
	if err != nil {
		t.Fatalf("promotionRanks: %v", err)
	}
	if ranks["lab"] != 1 || ranks["east"] != 2 {
		t.Errorf("ranks from spec = %v, want lab=1 east=2", ranks)
	}
}

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

	lz, present, err := loadSpec()
	if !present || err != nil {
		t.Fatalf("loadSpec present=%v err=%v", present, err)
	}
	var rerr error
	out := captureStdout(t, func() {
		rerr = runRenderDiff(lz, []string{"lab"}, "terraform-iac-bootstrap", "apl-values", false)
	})
	if rerr != nil {
		t.Fatalf("runRenderDiff: %v", rerr)
	}
	if !strings.Contains(out, "+ new") || !strings.Contains(out, "would change") {
		t.Errorf("diff should report new files:\n%s", out)
	}
}

func TestLineDiff(t *testing.T) {
	// Localized change → shows the -/+ pair with context, no truncation note.
	d := lineDiff("a\nb\nc\n", "a\nB\nc\n")
	if !strings.Contains(d, "- b") || !strings.Contains(d, "+ B") {
		t.Errorf("lineDiff missing the change:\n%s", d)
	}
	if strings.Contains(d, "more changes") {
		t.Errorf("small diff should not truncate:\n%s", d)
	}
	// New file (old empty) → all additions.
	if d := lineDiff("", "x\ny\n"); !strings.Contains(d, "+ x") || !strings.Contains(d, "+ y") {
		t.Errorf("new-file diff wrong:\n%s", d)
	}
}

// #4: the components registry view is accurate.
func TestComponentsRegistryView(t *testing.T) {
	byName := map[string]clusterspec.Component{}
	for _, c := range clusterspec.Components {
		byName[c.Name] = c
	}
	if got := componentDefault(byName["argocd"]); got != "on (required)" {
		t.Errorf("argocd default = %q", got)
	}
	if got := componentDefault(byName["harbor"]); got != "on" {
		t.Errorf("harbor default = %q", got)
	}
	if b := byName["observability"].Backends(); strings.Join(b, ",") != "apl-core,llz-argo" {
		t.Errorf("observability backends = %v, want apl-core,llz-argo", b)
	}
	if k := clusterspec.ComponentKnobs("observability"); strings.Join(k, ",") != "retention,storage,replicas" {
		t.Errorf("observability knobs = %v", k)
	}
}
