package main

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func TestClassifyRepoFile(t *testing.T) {
	cases := map[string]repoFileKind{
		"main.tf":               fileTerraform,
		"modules/vpc/main.tf":   fileTerraform,
		"envs/prod.tfvars":      fileTFVars,
		"vars.tfvars.json":      fileTFVars,
		"k8s/deploy.yaml":       fileYAML,
		"svc.yml":               fileYAML,
		"charts/app/Chart.yaml": fileHelmChart,
		"README.md":             fileOther,
		"scripts/run.sh":        fileOther,
	}
	for p, want := range cases {
		if got := classifyRepoFile(p); got != want {
			t.Errorf("classifyRepoFile(%q)=%v, want %v", p, got, want)
		}
	}
}

func TestParseTerraformHCL(t *testing.T) {
	src := `
provider "linode" {}
resource "linode_lke_cluster" "this" {}
resource "linode_instance" "a" {}
resource "linode_instance" "b" {}
data "linode_region" "r" {}
module "vpc" { source = "./modules/vpc" }
# resource "commented_out" "x" {}   <- must be ignored
`
	res, ds, mods, provs := parseTerraformHCL(src)
	if res["linode_lke_cluster"] != 1 || res["linode_instance"] != 2 {
		t.Errorf("resources=%v", res)
	}
	if _, ok := res["commented_out"]; ok {
		t.Error("commented-out resource must not be counted")
	}
	if ds["linode_region"] != 1 {
		t.Errorf("dataSources=%v", ds)
	}
	if !reflect.DeepEqual(mods, []string{"vpc"}) {
		t.Errorf("modules=%v", mods)
	}
	if !reflect.DeepEqual(provs, []string{"linode"}) {
		t.Errorf("providers=%v", provs)
	}
}

func TestDecodeYAMLDocsAndKubeResource(t *testing.T) {
	multi := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: team-demo
---
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: team-demo
`
	docs := decodeYAMLDocs(multi)
	if len(docs) != 2 {
		t.Fatalf("got %d docs, want 2", len(docs))
	}
	r, ok := kubeResourceFromDoc(docs[0])
	if !ok || r.Kind != "Deployment" || r.Namespace != "team-demo" {
		t.Errorf("kubeResource=%+v ok=%v", r, ok)
	}

	// A plain values doc (no apiVersion/kind) is not a kube resource.
	plain := decodeYAMLDocs("foo: bar\nbaz: 1\n")
	if _, ok := kubeResourceFromDoc(plain[0]); ok {
		t.Error("non-manifest YAML must not be a kube resource")
	}
}

func TestParseChartName(t *testing.T) {
	if got := parseChartName("apiVersion: v2\nname: my-app\nversion: 1.2.3\n"); got != "my-app" {
		t.Errorf("got %q, want my-app", got)
	}
}

func TestExtractAplSignals(t *testing.T) {
	// teamConfig as a mapping; apps with enabled flags; cluster.domainSuffix.
	docs := decodeYAMLDocs(`
teamConfig:
  demo: {}
  payments: {}
  admin: {}
apps:
  harbor:
    enabled: true
  gitea:
    enabled: false
cluster:
  domainSuffix: apl.example.com
`)
	var sig aplSignals
	for _, d := range docs {
		extractAplSignals(d, &sig)
	}
	sig.Teams = dedupeSorted(sig.Teams)
	sig.EnabledApps = dedupeSorted(sig.EnabledApps)
	sig.DisabledApps = dedupeSorted(sig.DisabledApps)
	sig.Domains = dedupeSorted(sig.Domains)

	if !reflect.DeepEqual(sig.Teams, []string{"demo", "payments"}) { // admin excluded
		t.Errorf("teams=%v", sig.Teams)
	}
	if !reflect.DeepEqual(sig.EnabledApps, []string{"harbor"}) {
		t.Errorf("enabledApps=%v", sig.EnabledApps)
	}
	if !reflect.DeepEqual(sig.DisabledApps, []string{"gitea"}) {
		t.Errorf("disabledApps=%v", sig.DisabledApps)
	}
	if !reflect.DeepEqual(sig.Domains, []string{"apl.example.com"}) {
		t.Errorf("domains=%v", sig.Domains)
	}
}

func TestExtractAplSignalsTeamsList(t *testing.T) {
	// teams as a list of {name: ...}.
	docs := decodeYAMLDocs("teams:\n  - name: alpha\n  - name: beta\n")
	var sig aplSignals
	extractAplSignals(docs[0], &sig)
	if !reflect.DeepEqual(dedupeSorted(sig.Teams), []string{"alpha", "beta"}) {
		t.Errorf("teams=%v", sig.Teams)
	}
}

func TestScanRepoTreeMixed(t *testing.T) {
	// An unstructured repo (the gsap case): TF + kube + helm + an Otomi values
	// file scattered with no fixed layout, plus dirs that must be pruned.
	fsys := fstest.MapFS{
		"infra/main.tf":               {Data: []byte(`provider "linode" {}` + "\n" + `resource "linode_lke_cluster" "c" {}`)},
		"infra/prod.tfvars":           {Data: []byte("region = \"us-ord\"\nnode_type = \"g6-standard-4\"\nnode_count = 5\n")},
		"deploy/app.yaml":             {Data: []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: team-demo\n")},
		"deploy/svc.yaml":             {Data: []byte("apiVersion: v1\nkind: Service\nmetadata:\n  name: web\n  namespace: team-demo\n")},
		"charts/app/Chart.yaml":       {Data: []byte("apiVersion: v2\nname: app\nversion: 0.1.0\n")},
		"charts/app/templates/d.yaml": {Data: []byte("kind: Deployment\nspec: {{ .Values.x }}\n")}, // helm template: unparseable, skipped
		"otomi/teams.yaml":            {Data: []byte("teamConfig:\n  demo: {}\n")},
		"README.md":                   {Data: []byte("# docs")},
		".git/config":                 {Data: []byte("[core]")},
		".terraform/lock.hcl":         {Data: []byte("ignored")},
	}
	inv := scanRepoTree(fsys)

	if inv.Terraform == nil {
		t.Fatal("expected terraform inventory")
	}
	if inv.Terraform.Resources["linode_lke_cluster"] != 1 {
		t.Errorf("tf resources=%v", inv.Terraform.Resources)
	}
	if inv.Terraform.Vars["region"] != "us-ord" || inv.Terraform.Vars["node_count"] != "5" {
		t.Errorf("tf vars=%v", inv.Terraform.Vars)
	}
	if inv.Kubernetes == nil {
		t.Fatal("expected kubernetes inventory")
	}
	if inv.Kubernetes.Kinds["Deployment"] != 1 || inv.Kubernetes.Kinds["Service"] != 1 {
		t.Errorf("kube kinds=%v", inv.Kubernetes.Kinds)
	}
	if !reflect.DeepEqual(inv.Kubernetes.Namespaces, []string{"team-demo"}) {
		t.Errorf("namespaces=%v", inv.Kubernetes.Namespaces)
	}
	if !reflect.DeepEqual(inv.Kubernetes.HelmCharts, []string{"app"}) {
		t.Errorf("helmCharts=%v", inv.Kubernetes.HelmCharts)
	}
	if inv.APL == nil || !reflect.DeepEqual(inv.APL.Teams, []string{"demo"}) {
		t.Errorf("apl=%+v", inv.APL)
	}
}

func TestScanRepoTreeEmpty(t *testing.T) {
	inv := scanRepoTree(fstest.MapFS{"README.md": {Data: []byte("nothing here")}})
	if inv.Terraform != nil || inv.Kubernetes != nil || inv.APL != nil {
		t.Errorf("empty repo should yield no inventory: %+v", inv)
	}
}

func TestRepoDriftWarnings(t *testing.T) {
	cluster := importCluster{Region: "us-ord", NodeCount: 5}
	repos := []repoInventory{
		{
			Role: "git", Path: "/clones/infra",
			Terraform: &terraformInventory{Vars: map[string]string{"region": "us-sea", "node_count": "3"}},
		},
		{
			Role: "apl", Path: "/clones/apl",
			APL: &aplSignals{EnabledApps: []string{"harbor", "gitea"}},
		},
	}
	// harbor's component is live; gitea's is not → only gitea drifts (+ region + node_count).
	live := map[string]bool{"harbor": true}
	w := repoDriftWarnings(cluster, []string{"harbor"}, live, repos)
	if len(w) != 3 {
		t.Fatalf("got %d warnings, want 3: %v", len(w), w)
	}
}

func TestRepoDriftDisabledButRunning(t *testing.T) {
	// trivy is declared disabled but detected running by name → flag it.
	// alertmanager is declared disabled and is only a sub-app of the running
	// observability component (not detected by name) → must NOT flag.
	repos := []repoInventory{{
		Role: "apl", Path: "/vals",
		APL: &aplSignals{DisabledApps: []string{"trivy", "alertmanager"}},
	}}
	live := map[string]bool{"observability": true, "imageScanning": true}
	w := repoDriftWarnings(importCluster{}, []string{"trivy"}, live, repos)
	if len(w) != 1 || !strings.Contains(w[0], `"trivy"`) || !strings.Contains(w[0], "disabled but it was detected running") {
		t.Fatalf("want exactly one trivy disabled-but-running warning, got %v", w)
	}
}

func TestRepoDriftNoFalsePositiveViaComponent(t *testing.T) {
	// APL declares loki+prometheus+otel enabled; none appear by name in liveDetected,
	// but observability IS a live component — so none should drift.
	repos := []repoInventory{{
		Role: "apl", Path: "/vals",
		APL: &aplSignals{EnabledApps: []string{"loki", "prometheus", "otel"}},
	}}
	live := map[string]bool{"observability": true}
	if w := repoDriftWarnings(importCluster{}, []string{"grafana"}, live, repos); len(w) != 0 {
		t.Errorf("expected no drift (observability is live), got %v", w)
	}
}

func TestRepoDriftWarningsNoLiveNoFalsePositives(t *testing.T) {
	// With no live cluster data, declared values can't be contradicted → no drift.
	repos := []repoInventory{{
		Terraform: &terraformInventory{Vars: map[string]string{"region": "us-sea", "node_count": "3"}},
		APL:       &aplSignals{EnabledApps: []string{"harbor"}},
	}}
	if w := repoDriftWarnings(importCluster{}, nil, nil, repos); len(w) != 0 {
		t.Errorf("no live data should yield no drift, got %v", w)
	}
}
