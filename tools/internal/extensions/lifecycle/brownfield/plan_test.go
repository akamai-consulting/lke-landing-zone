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
		// migrateAt >= 0 first: with both at -1 the `>` comparison is FALSE, so
		// deleting the MIGRATE section entirely would satisfy this gate. "Neither
		// heading precedes it" is not the same as "the right heading does".
		if migrateAt < 0 || cacheAt > migrateAt {
			t.Errorf("%s is not under the MIGRATE heading (migrateAt=%d cacheAt=%d) — a self-managed "+
				"database (or one whose engine could not be identified) told to rebuild rather than "+
				"migrate is data loss by documentation", name, migrateAt, cacheAt)
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

// TestAnUnrecognisedStatefulSetReachesThePlan.
//
// planDatabases has a branch that reads "engine not identified — inspect it
// before deciding anything", and until detectDBWorkloads emitted unmatched
// StatefulSets that branch could not be reached from a real scan: dbInfo was
// appended only when dbEngineForImages had already named the engine. A valkey,
// cassandra or etcd StatefulSet holding production data was therefore not
// mis-classified, it was ABSENT — and an operator working the plan migrates
// nothing they were never told about. The sibling test above hand-built its
// `legacy-store` fixture, so it proved the renderer and nothing upstream of it.
func TestAnUnrecognisedStatefulSetReachesThePlan(t *testing.T) {
	got := map[string]string{}
	for _, d := range detectDBWorkloads([]workload{
		{Namespace: "team-payments", Name: "ledger", Kind: "StatefulSet", Images: []string{"docker.io/cockroachdb/cockroach:v23"}},
		{Namespace: "team-payments", Name: "sessions", Kind: "StatefulSet", Images: []string{"docker.io/valkey/valkey:8"}},
		{Namespace: "team-payments", Name: "web", Kind: "Deployment", Images: []string{"nginx:1.27"}},
	}) {
		got[d.Name] = d.Engine
	}
	if _, ok := got["ledger"]; !ok {
		t.Error("a StatefulSet whose engine this scanner cannot name must still reach the plan — " +
			"absent from the report is absent from the migration, and nobody reviews what they cannot see")
	}
	if got["ledger"] != "" {
		t.Errorf("an unrecognised engine must be reported as unknown, not guessed at: %q", got["ledger"])
	}
	if got["sessions"] != "valkey" {
		t.Errorf("valkey is what a modern chart installs in place of Redis; engine=%q", got["sessions"])
	}
	if _, ok := got["web"]; ok {
		t.Error("a stateless Deployment must not be listed as a database to migrate")
	}
}
