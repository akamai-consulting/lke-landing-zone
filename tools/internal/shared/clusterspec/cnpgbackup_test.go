package clusterspec

import (
	"os"
	"regexp"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

func cnpgOn(t ComponentToggle) map[string]ComponentToggle {
	t.Enabled = boolPtr(true)
	return map[string]ComponentToggle{CnpgBackupComponent: t}
}

func TestCnpgBackupKnobsValidate(t *testing.T) {
	ok := []ComponentToggle{
		{},
		{Retention: "7d"},
		{Retention: "4w"},
		{Retention: "3m"}, // months, in barman's grammar
		{BackupIntervalHours: intp(6), Retention: "1d"},
		{BackupDatabases: "keycloak"},
		{BackupDatabases: "harbor keycloak"},
		{BackupGeneration: intp(2)},
	}
	for _, tog := range ok {
		if errs := validateComponentSizing("e", CnpgBackupComponent, tog); len(errs) != 0 {
			t.Errorf("%+v refused: %v", tog, errs)
		}
	}
	bad := map[string]ComponentToggle{
		"hours are not barman's":       {Retention: "720h"},
		"years are not barman's":       {Retention: "1y"},
		"zero":                         {Retention: "0d"},
		"shorter than two runs":        {Retention: "1d", BackupIntervalHours: intp(24)},
		"over a year":                  {Retention: "13m"},
		"interval not a divisor of 24": {BackupIntervalHours: intp(5)},
		"gitea is disabled everywhere": {BackupDatabases: "gitea"},
		"unknown database":             {BackupDatabases: "keycloak postgres"},
		"generation zero":              {BackupGeneration: intp(0)},
		"generation absurd":            {BackupGeneration: intp(1000)},
		"backupKeep is openbao's":      {BackupKeep: intp(3)},
		"namespaces are velero's":      {BackupExcludeNamespaces: "x"},
	}
	for why, tog := range bad {
		if errs := validateComponentSizing("e", CnpgBackupComponent, tog); len(errs) == 0 {
			t.Errorf("%s: %+v accepted", why, tog)
		}
	}
	// The gitea refusal must say WHY — "unknown" would send the author looking for
	// a typo in a database that is real elsewhere.
	errs := validateComponentSizing("e", CnpgBackupComponent, ComponentToggle{BackupDatabases: "gitea"})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "disables on every cluster") {
		t.Errorf("gitea refusal = %v", errs)
	}
	// And cnpgBackup's knobs on another component are a mistake, not a no-op.
	if errs := validateComponentSizing("e", "velero", ComponentToggle{BackupDatabases: "keycloak"}); len(errs) == 0 {
		t.Error("backupDatabases on velero was accepted")
	}
}

// An explicit harbor database where Harbor is off is refused at validation; the
// DEFAULT set simply leaves it out (TestRenderPlatformBackupsOverlay).
func TestCnpgBackupRefusesHarborWithoutHarbor(t *testing.T) {
	comps := cnpgOn(ComponentToggle{BackupDatabases: "harbor"})
	comps["harbor"] = ComponentToggle{Enabled: boolPtr(false)}
	comps["observability"] = ComponentToggle{Enabled: boolPtr(true)}
	var found bool
	for _, err := range validateComponents("e", comps) {
		found = found || strings.Contains(err.Error(), "components.harbor is disabled")
	}
	if !found {
		t.Errorf("harbor requested with harbor off was accepted: %v", validateComponents("e", comps))
	}
	comps["harbor"] = ComponentToggle{Enabled: boolPtr(true)}
	for _, err := range validateComponents("e", comps) {
		if strings.Contains(err.Error(), "components.harbor is disabled") {
			t.Errorf("harbor on, still refused: %v", err)
		}
	}
}

func TestCnpgRetentionAndSchedule(t *testing.T) {
	for in, want := range map[string]int{"7d": 7, "2w": 14, "3m": 90} {
		if got, ok := CnpgRetentionDays(in); !ok || got != want {
			t.Errorf("CnpgRetentionDays(%q) = %d,%v want %d", in, got, ok, want)
		}
	}
	if CnpgBackupSchedule(24) != "7 1 * * *" || CnpgBackupSchedule(6) != "7 */6 * * *" {
		t.Errorf("schedules: %q %q", CnpgBackupSchedule(24), CnpgBackupSchedule(6))
	}
	// Off velero's :37 and openbaoBackup's :23, so the three do not stack.
	if cnpgMinute == veleroMinute {
		t.Error("cnpg and velero backups start on the same minute")
	}
}

func parsePlatformBackups(t *testing.T, s string) map[string]map[string]any {
	t.Helper()
	d, err := PlatformBackupsDesired([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRenderPlatformBackupsOverlay(t *testing.T) {
	boot := Bootstrap{ManagedApps: DefaultManagedApps}
	if got := RenderPlatformBackupsOverlayEnv(boot, map[string]ComponentToggle{}); got != "" {
		t.Errorf("component off rendered a file — that would ASSERT backups:\n%s", got)
	}

	d := parsePlatformBackups(t, RenderPlatformBackupsOverlayEnv(boot, cnpgOn(ComponentToggle{})))
	if len(d) != 2 || d["keycloak"] == nil || d["harbor"] == nil {
		t.Fatalf("default set = %v, want harbor + keycloak", d)
	}
	kc := d["keycloak"]
	if kc["enabled"] != true || kc["schedule"] != "7 1 * * *" || kc["retentionPolicy"] != "30d" || kc["pathSuffix"] != "keycloak-1" {
		t.Errorf("keycloak defaults = %v", kc)
	}

	// Harbor not run here → the default set drops its database.
	noHarbor := cnpgOn(ComponentToggle{})
	noHarbor["harbor"] = ComponentToggle{Enabled: boolPtr(false)}
	if d := parsePlatformBackups(t, RenderPlatformBackupsOverlayEnv(boot, noHarbor)); len(d) != 1 || d["keycloak"] == nil {
		t.Errorf("harbor off: %v, want keycloak only", d)
	}
	// Harbor enabled but not declared in managedApps → not emitted on managed → same.
	if d := parsePlatformBackups(t, RenderPlatformBackupsOverlayEnv(Bootstrap{ManagedApps: []string{"loki"}}, cnpgOn(ComponentToggle{}))); d["harbor"] != nil {
		t.Errorf("harbor not in managedApps, still asked apl-core to back its database up: %v", d)
	}

	knobs := cnpgOn(ComponentToggle{BackupIntervalHours: intp(12), Retention: "2w", BackupDatabases: "keycloak", BackupGeneration: intp(3)})
	d = parsePlatformBackups(t, RenderPlatformBackupsOverlayEnv(boot, knobs))
	if len(d) != 1 || d["keycloak"]["schedule"] != "7 */12 * * *" || d["keycloak"]["retentionPolicy"] != "2w" || d["keycloak"]["pathSuffix"] != "keycloak-3" {
		t.Errorf("knobs = %v", d)
	}
}

// THE UPSTREAM CONTRACT, pinned against apl-core's own fixture
// (tests/fixtures/env/settings/platformBackups.yaml at main b7da647, copied to
// testdata). The CR identity and the four per-database keys LLZ writes must be the
// ones apl-core's fixture uses, and every rendered value must satisfy the schema
// constraint apl-core puts on it (values-schema.yaml: pathSuffix pattern) — a value
// the schema rejects converges silently on the machine branch.
func TestPlatformBackupsMatchesAplCoreShape(t *testing.T) {
	raw, err := os.ReadFile("testdata/aplcore-platformBackups-fixture.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var fixture, ours struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec struct {
			Database map[string]map[string]any `yaml:"database"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	rendered := RenderPlatformBackupsOverlayEnv(Bootstrap{ManagedApps: DefaultManagedApps}, cnpgOn(ComponentToggle{}))
	if err := yaml.Unmarshal([]byte(rendered), &ours); err != nil {
		t.Fatal(err)
	}
	if ours.Kind != fixture.Kind || ours.Metadata.Name != fixture.Metadata.Name {
		t.Errorf("rendered %s/%s, apl-core keeps %s/%s", ours.Kind, ours.Metadata.Name, fixture.Kind, fixture.Metadata.Name)
	}
	pathSuffix := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])$`)
	for db, keys := range ours.Spec.Database {
		up, ok := fixture.Spec.Database[db]
		if !ok {
			t.Errorf("apl-core's fixture has no spec.database.%s", db)
			continue
		}
		for k := range keys {
			if _, ok := up[k]; !ok {
				t.Errorf("spec.database.%s.%s is not a key apl-core's fixture uses", db, k)
			}
		}
		if s, _ := keys["pathSuffix"].(string); !pathSuffix.MatchString(s) {
			t.Errorf("pathSuffix %q fails apl-core's schema pattern", s)
		}
		if _, ok := CnpgRetentionDays(keys["retentionPolicy"].(string)); !ok {
			t.Errorf("retentionPolicy %v is not barman's grammar", keys["retentionPolicy"])
		}
	}
}

// The key-level merge: LLZ's four keys land, everything of apl-core's stays.
func TestSetPlatformBackupsIsKeyLevel(t *testing.T) {
	current := `kind: AplBackupCollection
metadata:
  name: platformBackups
spec:
  database:
    gitea:
      enabled: false
      pathSuffix: gitea
    keycloak:
      enabled: false
      pathSuffix: keycloak
      retentionPolicy: 7d
      schedule: 0 0 * * *
  gitea:
    enabled: true
    retentionPolicy: 7d
    schedule: 0 0 * * *
`
	desired := parsePlatformBackups(t, RenderPlatformBackupsOverlayEnv(Bootstrap{ManagedApps: DefaultManagedApps},
		cnpgOn(ComponentToggle{BackupDatabases: "keycloak"})))
	out, changed, err := SetPlatformBackups([]byte(current), desired)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	spec := m["spec"].(map[string]any)
	db := spec["database"].(map[string]any)
	kc := db["keycloak"].(map[string]any)
	if kc["enabled"] != true || kc["pathSuffix"] != "keycloak-1" || kc["retentionPolicy"] != "30d" {
		t.Errorf("keycloak after merge = %v", kc)
	}
	if g := db["gitea"].(map[string]any); g["pathSuffix"] != "gitea" || g["enabled"] != false {
		t.Errorf("the merge rewrote gitea's database entry, which LLZ has no opinion on: %v", g)
	}
	if g, ok := spec["gitea"].(map[string]any); !ok || g["enabled"] != true {
		t.Errorf("the merge dropped apl-core's gitea application backup: %v", spec["gitea"])
	}
	if m["kind"] != "AplBackupCollection" {
		t.Error("the merge lost the CR kind")
	}
	// Already at the desired state → no change, so no commit every pass.
	if _, changed, err := SetPlatformBackups(out, desired); err != nil || changed {
		t.Errorf("second pass changed=%v err=%v, want a no-op", changed, err)
	}
	// An empty desired set changes nothing.
	if _, changed, _ := SetPlatformBackups([]byte(current), nil); changed {
		t.Error("an empty desired set changed the CR")
	}
}

// buckets.cnpg is what apl-core turns into every database's destinationPath; it
// must name the bucket the module creates and the platform key is scoped to.
func TestObjOverlayNamesTheCnpgBucket(t *testing.T) {
	var doc objOverlayDoc
	if err := yaml.Unmarshal([]byte(RenderObjOverlayEnv("acme", "prod", "us-ord-10")), &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Spec.Provider.Linode.Buckets["cnpg"]; got != ObjCnpgBucket("acme", "prod") || got == "" {
		t.Errorf("buckets.cnpg = %q, want %q", got, ObjCnpgBucket("acme", "prod"))
	}
}

// The registry's database list must hold the cluster names apl-core's chart
// renders — Keycloak's Cluster is keycloak-db, not its release name.
func TestCnpgDatabasesNameTheLiveClusters(t *testing.T) {
	kc, _ := LookupCnpgDatabase("keycloak")
	hb, _ := LookupCnpgDatabase("harbor")
	if kc.Cluster != "keycloak-db" || kc.Namespace != "keycloak" || hb.Cluster != "harbor-otomi-db" || hb.Namespace != "harbor" {
		t.Errorf("databases = %+v %+v", kc, hb)
	}
	if _, ok := LookupCnpgDatabase("gitea"); ok {
		t.Error("gitea is in the database list, but LLZ disables gitea everywhere")
	}
}
