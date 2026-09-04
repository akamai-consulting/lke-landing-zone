package brownfield

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
	o := reportToEnvSpec(Deps{DefaultAplChartVersion: "6.1.0"}, initFixture())
	if o.Region != "us-ord" {
		t.Errorf("region=%q", o.Region)
	}
	if o.ClusterDomain != "lke579582.akamai-apl.net" {
		t.Errorf("clusterDomain=%q", o.ClusterDomain)
	}
	if o.ObjCluster != "us-ord-1" { // APL objectRegion preferred over Linode bucket region
		t.Errorf("objCluster=%q, want us-ord-1", o.ObjCluster)
	}
	if o.NodeType != "g8-dedicated-16-4" || o.NodeCount != "4" { // largest pool
		t.Errorf("nodeType=%q count=%q", o.NodeType, o.NodeCount)
	}
	// UNSET, DELIBERATELY. An omitted pin resolves to
	// clusterspec.BaselineAplChartVersion and keeps resolving to it across every
	// future bump, which is what "tracks the baseline" has to mean for an instance
	// nobody hand-edits. Seeding the baseline as a literal here — which this did —
	// made every imported instance born carrying a pin `llz upgrade` then deletes,
	// so the scaffold was manufacturing the stale field the upgrade lever exists to
	// retire. The version is still REPORTED to the operator; it is just not written
	// into the spec.
	// The field is GONE from EnvSpec entirely, which is stronger than asserting it
	// is empty: this struct's header says a field appearing here is a claim that
	// adoption can discover it, and after the seed was removed nothing wrote this
	// one. A dead field that import.go still mapped into envdef.Opts was a claim
	// with no source behind it, so it was deleted rather than left reading "".
	// k8sVersion is not a field of EnvSpec at all: the source cluster's version is
	// never a valid LKE target, so "must not be copied" is now structural rather
	// than asserted.
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
	md := buildMigrationTodo(Deps{}, initFixture(), "prod")
	mustContain := []string{
		"apl-core " + "",                       /* was importInitAplChartVersion; now Deps.DefaultAplChartVersion */ // target version stated (tracks the baseline)
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
