package main

import (
	"reflect"
	"strings"
	"testing"
)

func initFixture() importReport {
	return importReport{
		Cluster: importCluster{KubernetesVersion: "v1.35.5", Region: "us-ord", NodeCount: 7, NodeType: "g6-standard-4"},
		DNS:     importDNS{DomainSuffix: "lke579582.akamai-apl.net"},
		Platform: importPlatform{
			AplVersion: "v4.14.1",
			Components: map[string]bool{"argocd": true, "harbor": true, "observability": true, "gitea": true},
			HelmReleases: []helmRelease{
				{Namespace: "harbor", Name: "harbor", Chart: "harbor", ChartVersion: "1.13.0"},
			},
		},
		Linode: &importLinode{
			Region: "us-ord",
			NodePools: []lkePool{
				{Type: "g8-dedicated-16-4", Count: 4},
				{Type: "g6-standard-4", Count: 3},
			},
			ObjectStorage: []lkeBucket{{Label: "lke579582-loki", Region: "us-ord"}},
		},
		Repos: []repoInventory{
			{Role: "apl", APL: &aplSignals{ObjectRegion: "us-ord-1", AplVersion: "v4.14.1", DisabledApps: []string{"alertmanager", "thanos"}}},
		},
		Storage: importStorage{Databases: []dbInfo{{Namespace: "gitea", Name: "gitea-db", Engine: "postgres", Kind: "CNPG"}}},
		Teams: []importTeam{{
			Name: "gsap", Namespace: "team-gsap", Workloads: 20,
			Images:     []string{"a:1", "b:2"},
			SecretRefs: []secretRef{{Name: "gitea-credentials"}, {Name: "harbor-pullsecret"}},
		}},
		Summary:  importSummary{PVCs: 94, TotalStorage: "157Gi"},
		Warnings: []string{"in-cluster Gitea detected — migrate repos"},
	}
}

func TestReportToEnvAddOpts(t *testing.T) {
	o := reportToEnvAddOpts(initFixture())
	if o.region != "us-ord" {
		t.Errorf("region=%q", o.region)
	}
	if o.clusterDomain != "lke579582.akamai-apl.net" {
		t.Errorf("clusterDomain=%q", o.clusterDomain)
	}
	if o.objCluster != "us-ord-1" { // APL objectRegion preferred over Linode bucket region
		t.Errorf("objCluster=%q, want us-ord-1", o.objCluster)
	}
	if o.nodeType != "g8-dedicated-16-4" || o.nodeCount != "4" { // largest pool
		t.Errorf("nodeType=%q count=%q", o.nodeType, o.nodeCount)
	}
	if o.aplChartVersion != "5.0.0" {
		t.Errorf("aplChartVersion=%q, want the migration target 5.0.0", o.aplChartVersion)
	}
	if o.k8sVersion != "" { // must NOT copy the source v1.35.5
		t.Errorf("k8sVersion should be left unset, got %q", o.k8sVersion)
	}
}

func TestLargestPoolFallbacks(t *testing.T) {
	// No Linode → fall back to kube-derived cluster pools.
	rep := importReport{Cluster: importCluster{NodePools: []nodePool{{NodeType: "small", Count: 2}, {NodeType: "big", Count: 9}}}}
	if nt, nc := largestPool(rep); nt != "big" || nc != 9 {
		t.Errorf("got %q/%d, want big/9", nt, nc)
	}
	// No pools at all → majority fields.
	rep2 := importReport{Cluster: importCluster{NodeType: "g6", NodeCount: 5}}
	if nt, nc := largestPool(rep2); nt != "g6" || nc != 5 {
		t.Errorf("got %q/%d, want g6/5", nt, nc)
	}
}

func TestEnabledComponentAssignments(t *testing.T) {
	got := enabledComponentAssignments(initFixture())
	// argocd is mandatory → excluded; rest sorted.
	want := []string{
		"components.gitea.enabled=true",
		"components.harbor.enabled=true",
		"components.observability.enabled=true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("assignments=%v, want %v", got, want)
	}
}

func TestBuildMigrationTodo(t *testing.T) {
	md := buildMigrationTodo(initFixture(), "prod")
	mustContain := []string{
		"apl-core 5.0.0",                       // target version stated
		"v4.14.1",                              // source version
		"k8s_version",                          // the leave-default flag
		"apiServerAllowCIDRs",                  // runner CIDRs manual
		"in-cluster Gitea detected",            // carried warning
		"gitea-credentials, harbor-pullsecret", // secret checklist
		"94 PersistentVolume",                  // data
		"gitea/gitea-db (postgres, CNPG)",      // database
		"team `gsap`: 20 workload(s)",          // workloads
		"harbor/harbor — harbor 1.13.0",        // helm reference
		"disabled in the source",               // coarser-component gap section
		"alertmanager, thanos",                 // the source's disabled apps
	}
	for _, s := range mustContain {
		if !strings.Contains(md, s) {
			t.Errorf("MIGRATION-TODO missing %q\n---\n%s", s, md)
		}
	}
}

func TestImportInitNoFlagsShowsHelp(t *testing.T) {
	cmd := importInitCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bare init should not error: %v", err)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("expected help, got:\n%s", out.String())
	}
}
