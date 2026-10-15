package clusterspec

// cnpgbackup.go — the cnpgBackup component: apl-core's OWN CloudNativePG backups
// of the Harbor and Keycloak databases, turned on and pointed at this
// deployment's cnpg bucket.
//
// LLZ RUNS NOTHING HERE. apl-core already knows how to back its databases up:
// its otomi-db chart renders a barman-cloud ObjectStore, a WAL-archiving plugin
// stanza on the Cluster and a ScheduledBackup whenever
// platformBackups.database.<db>.enabled is true and obj.provider.type is linode
// (values/<db>/<db>-otomi-db.gotmpl). What it does not have on a managed App
// Platform is anyone to switch that on, a bucket to send it to, or a gate that
// the backups land. This file is the first two, as two overlay fragments the
// apl-overlay reconciler already carries:
//
//   - obj.yaml gains buckets.cnpg (RenderObjOverlayEnv), so the destinationPath
//     apl-core builds is s3://<prefix>-cnpg-<env>/<pathSuffix>;
//   - platformBackups.yaml is apl-core's AplBackupCollection settings CR
//     (env/settings/platformBackups.yaml), with spec.database.<db>.{enabled,
//     schedule, retentionPolicy, pathSuffix} for each database LLZ backs up.
//
// The third is `llz ci assert-cnpg-backup` (lifecycle/cnpgbackup), which reads the
// LIVE Cluster and ObjectStore back rather than this render.
//
// One file, because the rules here have three readers that must not each hold a
// copy — Validate (what an author may write), the renderer (what apl-core is
// told) and the gate (which database it expects to find backing up where).

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	// CnpgBackupComponent is the spec.components key.
	CnpgBackupComponent = "cnpgBackup"
	// CnpgBackupNamespace is the carved App's namespace. It holds the alert rules
	// and nothing else: the databases are apl-core's, in their own namespaces.
	CnpgBackupNamespace = "llz-cnpg-backup"
	// CnpgBackupRuleGroup is the PrometheusRule group the component ships. The
	// gate requires Prometheus to have loaded it.
	CnpgBackupRuleGroup = "llz-cnpg-backup"

	// OverlayPlatformBackupsFile is the per-env overlay file and, under
	// env/settings/, apl-core's own settings path for the same CR.
	OverlayPlatformBackupsFile = "platformBackups.yaml"
	// platformBackupsKind / platformBackupsName are the CR identity apl-core keeps
	// env/settings/platformBackups.yaml as — confirmed against apl-core main
	// (b7da647): tests/fixtures/env/settings/platformBackups.yaml, and the
	// AplBackupCollection FileMap in src/common/repo.ts.
	platformBackupsKind = "AplBackupCollection"
	platformBackupsName = "platformBackups"

	// CnpgBarmanPlugin is the CNPG-i plugin apl-core's chart wires when
	// apps.cnpg.useBackupPlugin is true — its default, and a v64 values migration
	// forces it on every existing instance.
	CnpgBarmanPlugin = "barman-cloud.cloudnative-pg.io"

	// CnpgBackupDefaultIntervalHours is one backup a day. The knob is
	// backupIntervalHours, shared with openbaoBackup and velero and bounded the
	// same way (a divisor of 24), so the staleness alert can assume a regular
	// interval no longer than a day.
	CnpgBackupDefaultIntervalHours = 24
	// CnpgBackupDefaultRetention is barman's retentionPolicy: thirty days of base
	// backups and the WAL between them — apl-core's own default is 7d. Thirty
	// matches velero's default TTL, so a point-in-time cluster restore has the
	// database state of the same day to go with it.
	CnpgBackupDefaultRetention = "30d"
	// CnpgBackupMaxRetentionDays bounds the policy at a year.
	CnpgBackupMaxRetentionDays = 366
	// CnpgBackupMaxGeneration bounds backupGeneration; anything near it is a typo.
	CnpgBackupMaxGeneration = 999

	// cnpgMinute is off the hour and off openbaoBackup's :23 and velero's :37.
	cnpgMinute = 7
	// cnpgDailyHour places the default daily run at 01:07 UTC — half an hour
	// before velero's 01:37, so a deployment running both has the database base
	// backup of the same night as the resource backup.
	cnpgDailyHour = 1
)

// CnpgDatabase is one apl-core database the component can back up.
type CnpgDatabase struct {
	// Key is the name in platformBackups.database.<key> and databases.<key>, and
	// the token backupDatabases takes.
	Key string
	// Namespace and Cluster are where apl-core runs it. Cluster is the CNPG
	// Cluster metadata.name — the otomi-db chart's `name` value, NOT its Helm
	// release name: Keycloak's release is keycloak-otomi-db but its Cluster is
	// keycloak-db (values/keycloak/keycloak-otomi-db.gotmpl; apl-core's own
	// migrate.ts selects cnpg.io/cluster=keycloak-db). The ObjectStore the chart
	// renders carries the same name.
	Namespace, Cluster string
	// Database / Owner are the application database and role apl-core's initdb
	// creates — what a databases.<key>.recovery block names.
	Database, Owner string
}

// CnpgDatabases is every database the component knows, in render order. Gitea is
// deliberately absent: LLZ disables apl-core's gitea on every cluster
// (aplStaticDisabledApps), so there is no gitea database to back up.
var CnpgDatabases = []CnpgDatabase{
	{Key: "keycloak", Namespace: "keycloak", Cluster: "keycloak-db", Database: "keycloak", Owner: "keycloak"},
	{Key: "harbor", Namespace: "harbor", Cluster: "harbor-otomi-db", Database: "registry", Owner: "harbor"},
}

// LookupCnpgDatabase returns the registry entry for key.
func LookupCnpgDatabase(key string) (CnpgDatabase, bool) {
	for _, d := range CnpgDatabases {
		if d.Key == key {
			return d, true
		}
	}
	return CnpgDatabase{}, false
}

// cnpgRetentionRe is barman's retentionPolicy grammar as CNPG enforces it
// (`^[1-9][0-9]*[dwm]$` — days, weeks, MONTHS). Note `m` is months here, where the
// generic durationRe reads it as minutes; this is the only check that applies.
var cnpgRetentionRe = regexp.MustCompile(`^([1-9][0-9]*)([dwm])$`)

// CnpgRetentionDays converts a retention policy to days (a month counted as 30).
func CnpgRetentionDays(r string) (int, bool) {
	m := cnpgRetentionRe.FindStringSubmatch(r)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	switch m[2] {
	case "w":
		n *= 7
	case "m":
		n *= 30
	}
	return n, true
}

// CnpgBackupSchedule is the 5-field cron for an interval of n hours. apl-core's
// chart prepends the seconds field CNPG's ScheduledBackup wants.
func CnpgBackupSchedule(n int) string {
	if n == 24 {
		return fmt.Sprintf("%d %d * * *", cnpgMinute, cnpgDailyHour)
	}
	return fmt.Sprintf("%d */%d * * *", cnpgMinute, n)
}

// CnpgPathSuffix is the object prefix a database archives under:
// <db>-<generation>. THE GENERATION IS THE RESTORE KNOB. A CNPG cluster refuses to
// archive WAL into a destination that already holds an archive
// (barman-cloud-check-wal-archive: "Expected empty archive"), so a database
// rebuilt or restored from generation N must write generation N+1 beside it —
// which is also what keeps the backup being restored from untouched.
func CnpgPathSuffix(db string, generation int) string {
	return fmt.Sprintf("%s-%d", db, generation)
}

// CnpgBackupSettings is what the component tells apl-core for one deployment.
type CnpgBackupSettings struct {
	Databases       []CnpgDatabase
	Schedule        string
	RetentionPolicy string
	Generation      int
}

// CnpgBackupSettingsFor resolves the toggle's knobs into what is rendered.
// harborAvailable is whether this deployment runs apl-core's Harbor at all
// (its component enabled and emitted): the Harbor database only exists there,
// so the DEFAULT set drops it rather than asking apl-core to back up a database
// it never created. An explicit `backupDatabases: harbor` on such a deployment is
// refused by Validate instead. Pure.
func CnpgBackupSettingsFor(t ComponentToggle, harborAvailable bool) CnpgBackupSettings {
	s := CnpgBackupSettings{
		Schedule:        CnpgBackupSchedule(CnpgBackupDefaultIntervalHours),
		RetentionPolicy: CnpgBackupDefaultRetention,
		Generation:      1,
	}
	if t.BackupIntervalHours != nil {
		s.Schedule = CnpgBackupSchedule(*t.BackupIntervalHours)
	}
	if t.Retention != "" {
		s.RetentionPolicy = t.Retention
	}
	if t.BackupGeneration != nil {
		s.Generation = *t.BackupGeneration
	}
	want := map[string]bool{}
	explicit := strings.Fields(t.BackupDatabases)
	for _, k := range explicit {
		want[k] = true
	}
	for _, d := range CnpgDatabases {
		switch {
		case len(explicit) > 0 && !want[d.Key]:
			continue
		case len(explicit) == 0 && d.Key == "harbor" && !harborAvailable:
			continue
		}
		s.Databases = append(s.Databases, d)
	}
	return s
}

// validateCnpgBackupKnobs bounds the component's knobs. Called from
// validateComponentSizing, which has already rejected knobs it does not read.
func validateCnpgBackupKnobs(env string, t ComponentToggle) []error {
	var errs []error
	at := func(knob string) string {
		return fmt.Sprintf("environments.%s.components.%s.%s", env, CnpgBackupComponent, knob)
	}
	interval := CnpgBackupDefaultIntervalHours
	if t.BackupIntervalHours != nil {
		interval = *t.BackupIntervalHours
	}
	if t.Retention != "" {
		days, ok := CnpgRetentionDays(t.Retention)
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%s %q is not a barman retention policy: a whole number of days, weeks or MONTHS "+
				"(7d, 4w, 3m — CNPG reads `m` as months, and has no hours)", at("retention"), t.Retention))
		case days*24 < 2*interval || days < 1:
			errs = append(errs, fmt.Errorf("%s %q is shorter than two backup intervals (%dh): barman would expire a base "+
				"backup before its successor existed, leaving windows with nothing to restore", at("retention"), t.Retention, 2*interval))
		case days > CnpgBackupMaxRetentionDays:
			errs = append(errs, fmt.Errorf("%s %q is longer than a year: the cnpg bucket is versioned, so cnpg_noncurrent_days "+
				"already keeps deleted backups on top of whatever is set here", at("retention"), t.Retention))
		}
	}
	if t.BackupGeneration != nil && (*t.BackupGeneration < 1 || *t.BackupGeneration > CnpgBackupMaxGeneration) {
		errs = append(errs, fmt.Errorf("%s %d must be between 1 and %d", at("backupGeneration"), *t.BackupGeneration, CnpgBackupMaxGeneration))
	}
	for _, k := range strings.Fields(t.BackupDatabases) {
		if k == "gitea" {
			errs = append(errs, fmt.Errorf("%s names gitea, which LLZ disables on every cluster (apl-core's gitea is not the "+
				"values-repo backend here — see aplStaticDisabledApps), so there is no gitea database to back up", at("backupDatabases")))
			continue
		}
		if _, ok := LookupCnpgDatabase(k); !ok {
			errs = append(errs, fmt.Errorf("%s: %q is not a database this component backs up (known: %s)", at("backupDatabases"), k, cnpgDatabaseKeys()))
		}
	}
	return errs
}

// validateCnpgBackupDependencies is the cross-component half: an explicit
// database whose app this deployment does not run. Called from
// validateComponents, which holds the whole toggle map.
func validateCnpgBackupDependencies(env string, components map[string]ComponentToggle) []error {
	var errs []error
	for _, k := range strings.Fields(components[CnpgBackupComponent].BackupDatabases) {
		if k == "harbor" && !ComponentEnabled(components, "harbor") {
			errs = append(errs, fmt.Errorf("environments.%s.components.%s.backupDatabases names harbor, but components.harbor is "+
				"disabled here, so apl-core runs no Harbor database to back up. Drop it from backupDatabases (the default "+
				"set already leaves it out where Harbor is off)", env, CnpgBackupComponent))
		}
	}
	return errs
}

func cnpgDatabaseKeys() string {
	keys := make([]string, len(CnpgDatabases))
	for i, d := range CnpgDatabases {
		keys[i] = d.Key
	}
	return strings.Join(keys, ", ")
}

// ── the platformBackups overlay ──────────────────────────────────────────────

type platformBackupsDoc struct {
	Kind     string              `yaml:"kind"`
	Metadata objMeta             `yaml:"metadata"`
	Spec     platformBackupsSpec `yaml:"spec"`
}

type platformBackupsSpec struct {
	Database map[string]platformBackupsDB `yaml:"database"`
}

// platformBackupsDB is exactly the four keys LLZ owns per database. Nothing
// else of apl-core's (recovery, externalClusters, the gitea application backup)
// is emitted, so the key-level merge leaves it standing.
type platformBackupsDB struct {
	Enabled         bool   `yaml:"enabled"`
	Schedule        string `yaml:"schedule"`
	RetentionPolicy string `yaml:"retentionPolicy"`
	PathSuffix      string `yaml:"pathSuffix"`
}

// RenderPlatformBackupsOverlayEnv is a deployment's per-env platformBackups.yaml,
// or "" when the component is off or not emitted — and "" means LLZ has NO
// OPINION, not "disable": the reconciler then leaves apl-core's own settings
// alone, so an operator who enabled a database backup in the Console keeps it.
// The flip side is that disabling the component stops LLZ asserting the backups
// rather than turning them off; the runbook says how to turn them off.
func RenderPlatformBackupsOverlayEnv(boot Bootstrap, components map[string]ComponentToggle) string {
	c, ok := LookupComponent(CnpgBackupComponent)
	if !ok || !ComponentEnabled(components, CnpgBackupComponent) || !c.EmitOnManaged(boot, components) {
		return ""
	}
	s := CnpgBackupSettingsFor(components[CnpgBackupComponent], harborEmitted(boot, components))
	if len(s.Databases) == 0 {
		return ""
	}
	dbs := map[string]platformBackupsDB{}
	for _, d := range s.Databases {
		dbs[d.Key] = platformBackupsDB{
			Enabled:         true,
			Schedule:        s.Schedule,
			RetentionPolicy: s.RetentionPolicy,
			PathSuffix:      CnpgPathSuffix(d.Key, s.Generation),
		}
	}
	return marshalYAML(platformBackupsDoc{
		Kind:     platformBackupsKind,
		Metadata: objMeta{Name: platformBackupsName},
		Spec:     platformBackupsSpec{Database: dbs},
	})
}

// CnpgBackupDatabasesFor is the set of databases a deployment's spec backs up —
// the same answer the renderer gives, for the gate to hold the live cluster to.
func CnpgBackupDatabasesFor(boot Bootstrap, components map[string]ComponentToggle) []CnpgDatabase {
	return CnpgBackupSettingsFor(components[CnpgBackupComponent], harborEmitted(boot, components)).Databases
}

// harborEmitted reports whether this deployment runs apl-core's Harbor: the
// component is enabled AND LLZ emits it (on managed it is conditional on
// `harbor` being declared in managedApps).
func harborEmitted(boot Bootstrap, components map[string]ComponentToggle) bool {
	h, ok := LookupComponent("harbor")
	return ok && ComponentEnabled(components, "harbor") && h.EmitOnManaged(boot, components)
}

// PlatformBackupsDesired reads the per-database keys out of a rendered
// platformBackups overlay source. An empty map (no error) means the source says
// nothing.
func PlatformBackupsDesired(src []byte) (map[string]map[string]any, error) {
	m, err := unmarshalMap(src)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	spec, _ := m["spec"].(map[string]any)
	db, _ := spec["database"].(map[string]any)
	for k, v := range db {
		if vm, ok := v.(map[string]any); ok && len(vm) > 0 {
			out[k] = vm
		}
	}
	return out, nil
}

// SetPlatformBackups asserts LLZ's per-database keys onto apl-core's OWN
// AplBackupCollection CR, leaving every other key in it untouched.
//
// KEY-LEVEL, NOT WHOLE-FILE, because this CR is co-written. apl-core models it
// as a settings resource of its own (repo.ts: resourceGroup platformBackups,
// pathGlob env/settings/platformBackups.yaml), which is the shape the App
// Platform Console's settings pages write through apl-api and apl-operator
// re-marshals; and it carries settings LLZ has no opinion on: the gitea
// application backup, and gitea's database entry. A wholesale write would blank them on every pass; writing only
// spec.database.<db>.{enabled,schedule,retentionPolicy,pathSuffix} for the
// databases LLZ backs up cannot.
//
// THE COST OF THAT CHOICE, stated: the Console can no longer change those four
// keys for those databases — the next pass reasserts the spec. That is the point
// of putting them in the spec, and the runbook names the knob to use instead.
//
// Returns changed=false when every key already matches, so the reconciler does
// not push a commit every pass against a file apl-core re-marshals.
func SetPlatformBackups(current []byte, desired map[string]map[string]any) (updated []byte, changed bool, err error) {
	if len(desired) == 0 {
		return current, false, nil
	}
	m, err := unmarshalMap(current)
	if err != nil {
		return nil, false, err
	}
	spec, ok := m["spec"].(map[string]any)
	if !ok {
		spec = map[string]any{}
		m["spec"] = spec
	}
	db, ok := spec["database"].(map[string]any)
	if !ok {
		db = map[string]any{}
		spec["database"] = db
	}
	for name, want := range desired {
		cur, ok := db[name].(map[string]any)
		if !ok {
			cur = map[string]any{}
			db[name] = cur
		}
		for k, v := range want {
			if fmt.Sprint(cur[k]) != fmt.Sprint(v) {
				cur[k] = v
				changed = true
			}
		}
	}
	if !changed {
		return current, false, nil
	}
	return marshalMap(m), true, nil
}
