package brownfield

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
		"Likely caches — VERIFY, then rebuild",                              // redis, stated as a default to check
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

// TestMigrationPlanDoesNotCallASelfManagedDatabaseEphemeral is the one finding in
// this file that could cost an operator their data.
//
// planDatabases bucketed everything that was not `Kind: "CNPG"` into "Caches —
// rebuild, do NOT migrate", each line reading "ephemeral; the new cluster
// provisions a fresh instance". detectDBWorkloads sets Kind: "workload" for EVERY
// self-managed database it finds — so a postgres:15 StatefulSet holding production
// data was handed to the operator as throwaway, in a document whose entire purpose
// is to be followed literally. The engine was recorded correctly the whole time
// and simply not consulted.
func TestMigrationPlanDoesNotCallASelfManagedDatabaseEphemeral(t *testing.T) {
	rep := planFixture()
	rep.Storage.Databases = append(rep.Storage.Databases,
		dbInfo{Namespace: "team-payments", Name: "orders-db", Kind: "workload", Engine: "postgres",
			Clients: []string{"Deployment/orders-api"}},
		dbInfo{Namespace: "team-payments", Name: "legacy-store", Kind: "workload"}, // engine unidentified
	)
	p := buildMigrationPlan(rep)

	for _, name := range []string{"orders-db", "legacy-store"} {
		i := strings.Index(p, name)
		if i < 0 {
			t.Fatalf("%s missing from the plan entirely", name)
		}
		// The section heading that precedes it decides what the operator does.
		before := p[:i]
		cacheAt := strings.LastIndex(before, "Likely caches")
		migrateAt := strings.LastIndex(before, "Self-managed databases — MIGRATE")
		if cacheAt > migrateAt {
			t.Errorf("%s is listed under the caches heading — a self-managed database (or one whose "+
				"engine could not be identified) told to rebuild rather than migrate is data loss by "+
				"documentation", name)
		}
	}
	if !strings.Contains(p, "Treat every one as holding data that matters") {
		t.Error("the self-managed section must say plainly that these hold real data")
	}
	// And the cache section must stop asserting ephemerality it cannot know.
	if strings.Contains(p, "ephemeral; the new cluster provisions a fresh instance") {
		t.Error("Redis with AOF/RDB persistence is a primary store, and this scan reads the image name, " +
			"not the configuration — it must ask the operator to verify, not assert")
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
