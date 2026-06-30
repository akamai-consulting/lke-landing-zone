package main

import (
	"strings"
	"testing"
)

func planFixture() importReport {
	return importReport{
		Linode: &importLinode{
			ObjectStorage: []lkeBucket{{Label: "lke579582-loki"}, {Label: "lke579582-harbor"}},
		},
		Repos: []repoInventory{{Role: "apl", APL: &aplSignals{ObjectRegion: "us-ord-1"}}},
		Storage: importStorage{
			Databases: []dbInfo{
				{Namespace: "keycloak", Name: "keycloak-db", Kind: "CNPG", Engine: "postgres", Clients: []string{"StatefulSet/keycloak-keycloakx"}},
				{Namespace: "harbor", Name: "harbor-otomi-db", Kind: "CNPG", Engine: "postgres", Clients: []string{"Deployment/harbor-core"}},
				{Namespace: "argocd", Name: "argocd-redis", Kind: "workload", Engine: "redis"},
			},
		},
	}
}

func TestBuildMigrationPlan(t *testing.T) {
	p := buildMigrationPlan(planFixture())
	mustContain := []string{
		"https://us-ord-1.linodeobjects.com",                                // source endpoint derived from objCluster
		"rclone config create src s3",                                       // rclone setup
		"rclone sync src:lke579582-loki dst:${DST_BUCKET_lke579582_loki",    // per-bucket, sanitized env key
		"rclone sync src:lke579582-harbor",                                  //
		"### keycloak/keycloak-db — client: StatefulSet/keycloak-keycloakx", // db + actual writer
		"Keycloak **realm export/import**",                                  // app-native hint
		"cnpg.io/instanceRole=primary",                                      // CNPG-aware dump
		"pg_dump -Fc -U postgres",                                           // fallback dump
		"Caches — rebuild, do NOT migrate",                                  // redis bucketed as cache
		"argocd/argocd-redis",                                               //
	}
	for _, s := range mustContain {
		if !strings.Contains(p, s) {
			t.Errorf("plan missing %q\n---\n%s", s, p)
		}
	}
	// A cache (workload-kind) must NOT get a CNPG dump block.
	if strings.Contains(p, "SRC_CLUSTER=argocd-redis") {
		t.Error("redis cache should not get a CNPG dump block")
	}
}

func TestReportBucketsFallbackToApl(t *testing.T) {
	// No Linode section → fall back to the APL values' bucket map.
	rep := importReport{Repos: []repoInventory{{Role: "apl", APL: &aplSignals{
		ObjectBuckets: map[string]string{"loki": "src-loki", "harbor": "src-harbor"},
	}}}}
	got := reportBuckets(rep)
	if len(got) != 2 || got[0] != "src-harbor" || got[1] != "src-loki" { // deduped + sorted
		t.Errorf("buckets=%v", got)
	}
}

func TestSanitizeEnvKey(t *testing.T) {
	if got := sanitizeEnvKey("lke579582-loki"); got != "lke579582_loki" {
		t.Errorf("got %q", got)
	}
}

func TestImportPlanNoFlagsShowsHelp(t *testing.T) {
	cmd := importPlanCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bare plan should not error: %v", err)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("expected help, got:\n%s", out.String())
	}
}
