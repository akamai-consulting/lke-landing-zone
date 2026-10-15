package cnpgbackup

// check.go — `llz ci assert-cnpg-backup`, the gate that fails when apl-core's
// database backups stop working.
//
// WHAT THE CHECK PROVES, for every database the spec backs up (keycloak, and
// harbor where Harbor runs), on every deployment that enables cnpgBackup:
//
//  1. the LIVE Cluster carries the barman-cloud plugin as its WAL archiver — read
//     off the cluster, not the render, so an overlay that never reached the
//     machine branch, or an apl-operator that never synced it, is caught where it
//     runs. The in-tree barmanObjectStore path is refused: LLZ's alerts read the
//     plugin's metrics and would be blind to it;
//  2. the ObjectStore the Cluster names writes to THIS deployment's cnpg bucket on
//     this deployment's endpoint — the bucket the platform key is scoped to and
//     the destroy guard protects — and, where objProxy is on, trusts its CA;
//  3. continuous WAL archiving is working (the Cluster's ContinuousArchiving
//     condition), which is the half of the RPO the schedule does not set;
//  4. an on-demand Backup, by the same method the ScheduledBackup uses, completes
//     with a backup id and an end WAL;
//  5. CONSUMER SIDE: through a temporary read-only key minted for the purpose, the
//     base backup (backup.info + data) is under <destinationPath>/<serverName>/
//     base/<backupId>/ and the end WAL segment under .../wals/ — the exact paths a
//     barman recovery reads, derived from the LIVE objects, so a restore looks
//     where the backup went; and the object's encryption matches objProxy (a
//     keyless GET refused where the proxy is on);
//  6. Prometheus has loaded the llz-cnpg-backup rule group, so the freshness
//     alerts can fire at all.
//
// --drill ADDS THE ROUND TRIP, on the e2e lane's deployments only (drill.go).
//
// WHAT STAYS GREEN WITHOUT IT: everything. The settings CR says enabled the moment
// the reconciler writes it; the Cluster goes healthy whether or not its archiver
// can reach the bucket; the ScheduledBackup exists whether or not a backup it
// takes can be restored, or lands where a restore would look.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/forge"
)

const (
	// E2ELaneBase is the deployment name the release-e2e lanes instantiate before
	// forge.LaneName makes it forge-unique.
	E2ELaneBase = "e2e"
	// createdBy marks every object this verb creates.
	createdBy = "llz-cnpg-backup-check"

	clusterKind     = "clusters.postgresql.cnpg.io"
	backupKind      = "backups.postgresql.cnpg.io"
	databaseKind    = "databases.postgresql.cnpg.io"
	objectStoreKind = "objectstores.barmancloud.cnpg.io"
)

// Budgets. Package vars so tests can shrink them.
var (
	// wireWait covers the whole chain from the overlay commit to a Cluster
	// carrying the plugin: the reconciler's pass, apl-operator's sync of the
	// apl-<env> branch, and the helm upgrade of the otomi-db release.
	wireWait     = 15 * time.Minute
	archiveWait  = 10 * time.Minute
	backupWait   = 15 * time.Minute
	canaryWait   = 5 * time.Minute
	restoreWait  = 20 * time.Minute
	verifyWait   = 5 * time.Minute
	nsGoneWait   = 5 * time.Minute
	pollInterval = 10 * time.Second
	now          = time.Now
	// runBudget bounds the whole verb BELOW the workflow step's 40m timeout, so a
	// slow cluster ends in this verb's own error rather than in GitHub killing the
	// job with no verdict (the bootstrap job's INVARIANT comment). The per-wait
	// budgets above can sum past it on a bad day; this is what stops them.
	runBudget = 38 * time.Minute
)

// target is what the spec says, resolved before touching anything.
type target struct {
	region, objCluster, bucket, endpointHost string
	// objProxy is whether the SSE-C gateway is on — and therefore whether the
	// backups must be ciphertext, and the drill must trust the proxy's CA.
	objProxy bool
	dbs      []clusterspec.CnpgDatabase
}

func resolve(region, what string) (target, bool, error) {
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return target{}, false, fmt.Errorf("assert-cnpg-backup: load spec: %w", err)
	}
	e, ok := lz.Env(region)
	if !ok {
		return target{}, false, fmt.Errorf("no deployment %q in the spec", region)
	}
	if !clusterspec.ComponentEnabled(e.Components, clusterspec.CnpgBackupComponent) {
		fmt.Printf("cnpgBackup is disabled for %s — LLZ asked apl-core for no database backups; skipping the %s.\n", region, what)
		return target{}, true, nil
	}
	oc := e.Cluster.ObjectStorage.Cluster
	if oc == "" {
		return target{}, false, fmt.Errorf("deployment %q declares no cluster.objectStorage.cluster", region)
	}
	dbs := clusterspec.CnpgBackupDatabasesFor(e.Cluster.Bootstrap, e.Components)
	if len(dbs) == 0 {
		// The component is on and names nothing: a green run here would have
		// examined nothing.
		return target{}, false, fmt.Errorf("cnpgBackup is enabled for %s but backs up no database (backupDatabases %q, harbor emitted=%v) — "+
			"refusing to pass having checked nothing", region, e.Components[clusterspec.CnpgBackupComponent].BackupDatabases,
			clusterspec.ComponentEnabled(e.Components, "harbor"))
	}
	proxy, _ := clusterspec.LookupComponent("objProxy")
	return target{
		region: region, objCluster: oc,
		bucket:       clusterspec.ObjCnpgBucket(lz.ObjLabelPrefix(), region),
		endpointHost: clusterspec.ObjEndpointHost(oc),
		objProxy:     clusterspec.ComponentEnabled(e.Components, "objProxy") && proxy.EmitOnManaged(e.Cluster.Bootstrap, e.Components),
		dbs:          dbs,
	}, false, nil
}

// Run is the verb: the backup check for every database, plus the round trip
// when drill is set.
func Run(ctx context.Context, region string, drill bool) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	ctx, cancel := context.WithTimeout(ctx, runBudget)
	defer cancel()
	if drill && !forge.IsLaneDeployment(E2ELaneBase, region) {
		return fmt.Errorf("refusing --drill: %q is not an e2e lane deployment (%s or a forge-suffixed %s-*). The drill creates a "+
			"canary database in the LIVE cluster and restores it into a scratch one; it is a release-e2e gate and runs nowhere "+
			"else. Without --drill this verb proves backups work on any deployment", region, E2ELaneBase, E2ELaneBase)
	}
	what := "backup check"
	if drill {
		what = "backup check and restore drill"
	}
	tg, skip, err := resolve(region, what)
	if err != nil || skip {
		return err
	}
	tok := firstNonEmpty(os.Getenv("LINODE_API_TOKEN"), os.Getenv("LINODE_TOKEN"))
	if tok == "" {
		return fmt.Errorf("LINODE_API_TOKEN is not set — the check reads the cnpg bucket through a temporary key it mints from it")
	}
	names := make([]string, len(tg.dbs))
	for i, d := range tg.dbs {
		names[i] = d.Key
	}
	fmt.Printf("checking the %s database backup(s) of %s into %s (objProxy %v)\n", strings.Join(names, " and "), region, tg.bucket, tg.objProxy)
	for _, db := range tg.dbs {
		if err := checkOne(ctx, tok, tg, db, drill); err != nil {
			return fmt.Errorf("%s database (%s/%s): %w", db.Key, db.Namespace, db.Cluster, err)
		}
	}
	if err := caps.Scrape("--monitors=", "--rule-groups", clusterspec.CnpgBackupRuleGroup); err != nil {
		return fmt.Errorf("the databases back up, but Prometheus has not loaded the %s rule group — every backup alert would "+
			"evaluate over nothing (check the PrometheusRule's prometheus=system label and the llz-cnpg-backup App): %w",
			clusterspec.CnpgBackupRuleGroup, err)
	}
	fmt.Printf("✓ %s's database backups work end to end: archived, backed up and readable where a restore looks in %s\n", region, tg.bucket)
	return nil
}

// checkOne is steps 1–5 for one database, and the drill when asked.
func checkOne(ctx context.Context, tok string, tg target, db clusterspec.CnpgDatabase, drill bool) error {
	w, err := waitWired(ctx, db)
	if err != nil {
		return err
	}
	fmt.Printf("✓ %s/%s archives through the %s plugin into ObjectStore %s (serverName %s)\n", db.Namespace, db.Cluster,
		clusterspec.CnpgBarmanPlugin, w.ObjectStore, w.ServerName)
	dest, err := readObjectStore(db, w.ObjectStore)
	if err != nil {
		return err
	}
	if err := DestinationVerdict(dest, tg.bucket, tg.endpointHost); err != nil {
		return err
	}
	if tg.objProxy {
		if err := ensureEndpointCA(db, w.ObjectStore, dest); err != nil {
			return err
		}
	}
	fmt.Printf("✓ ObjectStore %s writes to %s on %s\n", w.ObjectStore, dest.Path, dest.EndpointURL)
	if err := waitArchiving(ctx, db); err != nil {
		return err
	}

	var canary string
	if drill {
		canary = "llz_canary_" + newNonce()
		defer dropCanary(db)
		if err := createCanary(ctx, db, canary); err != nil {
			return err
		}
	}

	name := fmt.Sprintf("%s-llz-check-%d", db.Cluster, now().Unix())
	st, err := takeBackup(ctx, db, name)
	if err != nil {
		return err
	}
	if err := BackupVerdict(db, name, st); err != nil {
		return err
	}
	fmt.Printf("✓ Backup %s/%s completed: id %s, WAL %s → %s\n", db.Namespace, name, st.BackupID, st.BeginWal, st.EndWal)

	store, revoke, err := caps.TempStore(ctx, caps.Keys(tok), "llz-cnpg-check-"+tg.region, tg.objCluster, tg.bucket, tg.endpointHost, "read_only")
	if err != nil {
		return err
	}
	defer revoke()
	if err := verifyInBucket(ctx, store, tg, dest, w.ServerName, st); err != nil {
		return err
	}
	if !drill {
		return nil
	}
	return restoreDrill(ctx, tg, db, w, dest, st, store, canary)
}

// ── the pure half ────────────────────────────────────────────────────────────

// Wiring is what the live Cluster says about its backups.
type Wiring struct {
	// ObjectStore is the barman-cloud ObjectStore the plugin archives to.
	ObjectStore string
	// ServerName is the folder under the destination the archive lives in: the
	// plugin's serverName parameter, else the Cluster's own name.
	ServerName string
	// ImageName is the Postgres image the drill's recovery must run (a restore
	// needs the same major version).
	ImageName string
}

// ParseWiring reads a live Cluster's backup wiring. wired=false (no error) is
// "not yet": apl-core has not rendered backups onto it, which settles as the
// overlay is delivered. An in-tree barmanObjectStore is a FINAL error.
func ParseWiring(clusterJSON []byte) (w Wiring, wired bool, err error) {
	var c struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			ImageName string `json:"imageName"`
			Plugins   []struct {
				Name          string            `json:"name"`
				IsWALArchiver *bool             `json:"isWALArchiver"`
				Enabled       *bool             `json:"enabled"`
				Parameters    map[string]string `json:"parameters"`
			} `json:"plugins"`
			Backup *struct {
				BarmanObjectStore map[string]any `json:"barmanObjectStore"`
			} `json:"backup"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(clusterJSON, &c); err != nil {
		return Wiring{}, false, fmt.Errorf("parse the Cluster: %w", err)
	}
	if c.Metadata.Name == "" {
		return Wiring{}, false, fmt.Errorf("the Cluster JSON carries no name — it was not read from the cluster")
	}
	for _, p := range c.Spec.Plugins {
		if p.Name != clusterspec.CnpgBarmanPlugin || (p.Enabled != nil && !*p.Enabled) {
			continue
		}
		if p.IsWALArchiver == nil || !*p.IsWALArchiver {
			continue
		}
		obj := p.Parameters["barmanObjectName"]
		if obj == "" {
			return Wiring{}, false, fmt.Errorf("the %s plugin stanza names no barmanObjectName — cannot tell where it archives", p.Name)
		}
		server := p.Parameters["serverName"]
		if server == "" {
			server = c.Metadata.Name
		}
		return Wiring{ObjectStore: obj, ServerName: server, ImageName: c.Spec.ImageName}, true, nil
	}
	if c.Spec.Backup != nil && len(c.Spec.Backup.BarmanObjectStore) > 0 {
		return Wiring{}, false, fmt.Errorf("the Cluster backs up through the IN-TREE barmanObjectStore (apps.cnpg.useBackupPlugin=false). " +
			"LLZ's llz-cnpg-backup alerts read the barman-cloud plugin's metrics and would be blind to it, so this gate refuses it " +
			"rather than pass a deployment no alert watches. apl-core's own default and its v64 values migration set " +
			"useBackupPlugin: true — set it back in apps/cnpg")
	}
	return Wiring{}, false, nil
}

// Destination is where an ObjectStore writes.
type Destination struct {
	// Path is spec.configuration.destinationPath verbatim: s3://<bucket>/<prefix>.
	Path, Bucket, Prefix string
	EndpointURL          string
	// EndpointCA is set when the store trusts a private CA (the objProxy bundle).
	EndpointCA bool
	// SidecarEnv is instanceSidecarConfiguration.env, copied onto the drill's own
	// store (apl-core sets the AWS checksum switches Linode needs there).
	SidecarEnv []any
}

// ParseObjectStore reads an ObjectStore's destination. Anything that is not an
// s3:// path with a bucket is "could not tell", never a guess.
func ParseObjectStore(storeJSON []byte) (Destination, error) {
	var o struct {
		Spec struct {
			Configuration struct {
				DestinationPath string         `json:"destinationPath"`
				EndpointURL     string         `json:"endpointURL"`
				EndpointCA      map[string]any `json:"endpointCA"`
			} `json:"configuration"`
			InstanceSidecarConfiguration struct {
				Env []any `json:"env"`
			} `json:"instanceSidecarConfiguration"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(storeJSON, &o); err != nil {
		return Destination{}, fmt.Errorf("parse the ObjectStore: %w", err)
	}
	cfg := o.Spec.Configuration
	u, err := url.Parse(cfg.DestinationPath)
	if err != nil || u.Scheme != "s3" || u.Host == "" {
		return Destination{}, fmt.Errorf("the ObjectStore's destinationPath %q is not s3://<bucket>/<prefix> — cannot tell where backups go", cfg.DestinationPath)
	}
	return Destination{
		Path: cfg.DestinationPath, Bucket: u.Host, Prefix: strings.Trim(u.Path, "/"),
		EndpointURL: cfg.EndpointURL, EndpointCA: len(cfg.EndpointCA) > 0,
		SidecarEnv: o.Spec.InstanceSidecarConfiguration.Env,
	}, nil
}

// DestinationVerdict holds the live destination to the spec: the bucket this
// deployment's module creates (and its platform key is scoped to, and the destroy
// guard protects), on this deployment's endpoint, under a prefix. Pure.
func DestinationVerdict(d Destination, wantBucket, wantHost string) error {
	if d.Bucket != wantBucket {
		return fmt.Errorf("apl-core writes these backups to bucket %q, but this deployment's cnpg bucket is %q — the bucket the "+
			"platform key is scoped to and the destroy guard protects. The obj overlay's buckets.cnpg (env/settings/obj.yaml on the "+
			"apl-<env> branch) is what apl-core builds the destination from", d.Bucket, wantBucket)
	}
	if d.Prefix == "" {
		return fmt.Errorf("the destination %q has no pathSuffix, so every database would archive into the bucket root and collide", d.Path)
	}
	u, err := url.Parse(d.EndpointURL)
	if err != nil || u.Host != wantHost {
		return fmt.Errorf("the ObjectStore's endpointURL %q is not this deployment's object-storage endpoint https://%s — the "+
			"bucket is pinned to that endpoint, and a backup sent elsewhere never arrives", d.EndpointURL, wantHost)
	}
	return nil
}

// ArchivingVerdict reads the Cluster's ContinuousArchiving condition. done=false
// with no error is "not yet"; a False condition is returned as the reason, with
// the one cause worth naming spelled out.
func ArchivingVerdict(clusterJSON []byte) (done bool, why string) {
	var c struct {
		Status struct {
			Conditions []struct {
				Type, Status, Reason, Message string
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(clusterJSON, &c); err != nil {
		return false, "the Cluster did not parse: " + err.Error()
	}
	for _, cond := range c.Status.Conditions {
		if cond.Type != "ContinuousArchiving" {
			continue
		}
		if cond.Status == "True" {
			return true, ""
		}
		return false, fmt.Sprintf("ContinuousArchiving=%s (%s): %s", cond.Status, orUnknown(cond.Reason), orUnknown(cond.Message))
	}
	return false, "no ContinuousArchiving condition yet"
}

// archiveHint turns the archiver's own message into the next step.
func archiveHint(why string) string {
	switch {
	case strings.Contains(why, "empty archive"):
		return " — the destination ALREADY HOLDS an archive from an earlier cluster of this database, and CNPG refuses to " +
			"archive over it. That archive is a backup: restore from it, or bump components.cnpgBackup.backupGeneration so this " +
			"cluster archives beside it (docs/runbooks/cnpg-backup-restore.md). On the e2e lane, drain_data_buckets on the " +
			"previous run's destroy empties it"
	case strings.Contains(strings.ToUpper(why), "CERTIFICATE_VERIFY_FAILED") || strings.Contains(why, "SSL"):
		return " — barman does not trust the endpoint's certificate: with objProxy on that is the proxy's, and the instance " +
			"pods need the ObjectStore's endpointCA (Kyverno policy cnpg-obj-proxy-ca) and a restart if they predate it"
	case strings.Contains(why, "AccessDenied") || strings.Contains(why, "403"):
		return " — the platform key cannot write the cnpg bucket. Its scope is the obj-platform rotation-table entry; the " +
			"rotator re-mints a key that lacks a bucket on its next tick (`llz ci rotate-linode-creds`)"
	}
	return ""
}

// BackupStatus is the slice of a CNPG Backup's status the verdict reads.
type BackupStatus struct {
	Phase      string `json:"phase"`
	Error      string `json:"error"`
	BackupID   string `json:"backupId"`
	BeginWal   string `json:"beginWal"`
	EndWal     string `json:"endWal"`
	Method     string `json:"method"`
	ServerName string `json:"serverName"`
}

// BackupVerdict fails anything but a completed backup that names its id and
// WAL range. A completed backup with no id cannot be located in the bucket, which
// is the same as not having one. Pure.
func BackupVerdict(db clusterspec.CnpgDatabase, name string, st BackupStatus) error {
	switch {
	case st.Phase != "completed":
		return fmt.Errorf("backup %s ended %s: %s — `kubectl -n %s get %s %s -o yaml`, and the instance pod's plugin-barman-cloud "+
			"container log for barman's own error", name, orUnknown(st.Phase), orUnknown(st.Error), db.Namespace, backupKind, name)
	case st.BackupID == "" || st.EndWal == "":
		return fmt.Errorf("backup %s completed without a backupId/endWal (id %q, endWal %q) — cannot tell where in the bucket it is",
			name, st.BackupID, st.EndWal)
	}
	return nil
}

// BucketVerdict judges the consumer-side listing of one backup: the base backup
// directory barman recovery reads (backup.info and the data tarball) and the
// segment that makes it consistent (endWal), both under the LIVE destination and
// serverName. Pure.
func BucketVerdict(d Destination, serverName string, st BackupStatus, baseKeys, walKeys []string) error {
	base := basePrefix(d, serverName, st.BackupID)
	var info, data bool
	for _, k := range baseKeys {
		switch {
		case k == base+"backup.info":
			info = true
		case strings.HasPrefix(k, base+"data.tar"):
			data = true
		}
	}
	if !info || !data {
		sort.Strings(baseKeys)
		return fmt.Errorf("%s holds no complete base backup under %s (backup.info %v, data.tar* %v; found %v) — CNPG reported the "+
			"backup completed, but a recovery from this destination would not find it", d.Bucket, base, info, data, baseKeys)
	}
	wal := walPrefix(d, serverName, st.EndWal) + st.EndWal
	for _, k := range walKeys {
		if strings.HasPrefix(k, wal) {
			return nil
		}
	}
	sort.Strings(walKeys)
	return fmt.Errorf("%s holds the base backup but not its end WAL segment %s (under %s: %v) — without it the backup cannot "+
		"be brought to a consistent state, so it does not restore", d.Bucket, st.EndWal, walPrefix(d, serverName, st.EndWal), walKeys)
}

// EncryptionVerdict judges a keyless GET of a backup object against what objProxy
// says it must be. Where the proxy is on, the object must refuse (400): a 200 is
// a database backup in plaintext that the proxy should have encrypted — barman
// went around it. Where it is off, the object is plaintext by construction (the
// at-rest registration says so) and a 200 is the expected answer. Anything else is
// "could not tell". Pure.
func EncryptionVerdict(objKey string, objProxy bool, status int, code string) error {
	switch {
	case objProxy && status == 400:
		return nil
	case objProxy && status/100 == 2:
		return fmt.Errorf("%s READ BACK WITHOUT AN SSE-C KEY (HTTP %d) although objProxy is on — this database backup is plaintext "+
			"at rest: barman's traffic did not go through the proxy (the CoreDNS rewrite, or the ObjectStore's endpoint)", objKey, status)
	case !objProxy && status/100 == 2:
		return nil
	case !objProxy && status == 400:
		return fmt.Errorf("%s refused a keyless read (HTTP 400 %s) although objProxy is off — something is encrypting these backups "+
			"that this deployment does not run, so a restore could not read them", objKey, orUnknown(code))
	}
	return fmt.Errorf("could not tell whether %s is readable as expected: a keyless GET answered HTTP %d (%s)", objKey, status, orUnknown(code))
}

func basePrefix(d Destination, serverName, backupID string) string {
	return d.Prefix + "/" + serverName + "/base/" + backupID + "/"
}

// walPrefix is barman's WAL directory for a segment: the first 16 hex digits
// (timeline + log) of its 24-character name.
func walPrefix(d Destination, serverName, wal string) string {
	dir := wal
	if len(wal) >= 16 {
		dir = wal[:16]
	}
	return d.Prefix + "/" + serverName + "/wals/" + dir + "/"
}

// BackupJSON is an on-demand Backup by the plugin — the method the
// ScheduledBackup apl-core renders uses. Pure.
func BackupJSON(db clusterspec.CnpgDatabase, name string) []byte {
	b, _ := json.Marshal(map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": name, "namespace": db.Namespace,
			"labels": map[string]any{"app.kubernetes.io/created-by": createdBy}},
		"spec": map[string]any{
			"cluster":             map[string]any{"name": db.Cluster},
			"method":              "plugin",
			"pluginConfiguration": map[string]any{"name": clusterspec.CnpgBarmanPlugin},
		},
	})
	return b
}

// ── the transport half ───────────────────────────────────────────────────────

func getJSON(ns, kind, name string) ([]byte, error) {
	return caps.Cluster.Run("-n", ns, "get", kind, name, "-o", "json")
}

func waitWired(ctx context.Context, db clusterspec.CnpgDatabase) (Wiring, error) {
	var w Wiring
	var last, final error
	err := poll(ctx, wireWait, func() (bool, error) {
		out, err := getJSON(db.Namespace, clusterKind, db.Cluster)
		if err != nil {
			last = err
			return false, nil
		}
		got, wired, err := ParseWiring(out)
		if err != nil {
			final = err // in-tree, or unreadable wiring: waiting will not fix it
			return false, err
		}
		w = got
		if !wired {
			last = fmt.Errorf("the Cluster carries no %s WAL-archiver plugin stanza", clusterspec.CnpgBarmanPlugin)
		}
		return wired, nil
	})
	if final != nil {
		return Wiring{}, fmt.Errorf("%s/%s: %w", db.Namespace, db.Cluster, final)
	}
	if err != nil {
		if last == nil {
			return Wiring{}, err
		}
		return Wiring{}, fmt.Errorf("%s/%s was never wired for backups within %s (%v). In order: env/settings/platformBackups.yaml on "+
			"the apl-<env> branch must carry spec.database.%s.enabled: true (the apl-overlay reconciler writes it — "+
			"llz_apl_overlay_platform_backups_target_unusable says if it could not), env/settings/obj.yaml must say "+
			"provider.type linode with buckets.cnpg, and apl-operator must have synced the %s-otomi-db release",
			db.Namespace, db.Cluster, wireWait, last, db.Key, db.Key)
	}
	return w, nil
}

func readObjectStore(db clusterspec.CnpgDatabase, name string) (Destination, error) {
	out, err := getJSON(db.Namespace, objectStoreKind, name)
	if err != nil {
		return Destination{}, fmt.Errorf("the Cluster archives to ObjectStore %s/%s, which cannot be read: %w", db.Namespace, name, err)
	}
	return ParseObjectStore(out)
}

// ensureEndpointCA is objProxy's half of the wiring. An ObjectStore admitted
// before the cnpg-obj-proxy-ca policy existed has no endpointCA; re-admitting it
// (an annotation is an UPDATE) lets Kyverno add it. Either way the gate FAILS
// when it was missing: instance pods built without the CA keep failing until
// they are restarted, and restarting a database is not this gate's call.
func ensureEndpointCA(db clusterspec.CnpgDatabase, name string, d Destination) error {
	if d.EndpointCA {
		return nil
	}
	stamp := fmt.Sprintf("llz.akamai-consulting.io/readmitted-at=%d", now().Unix())
	if out, err := caps.Writer.Annotate(db.Namespace, objectStoreKind, name, stamp); err != nil {
		return fmt.Errorf("objProxy is on but ObjectStore %s/%s has no endpointCA, and re-admitting it failed: %w\n%s", db.Namespace, name, err, out)
	}
	again, err := readObjectStore(db, name)
	if err != nil {
		return err
	}
	if !again.EndpointCA {
		return fmt.Errorf("objProxy is on but ObjectStore %s/%s has no endpointCA even after re-admission — the Kyverno policy "+
			"cnpg-obj-proxy-ca did not mutate it (`kubectl get clusterpolicy cnpg-obj-proxy-ca`; is Kyverno running?). barman "+
			"cannot trust the proxy without it, so every archive and backup fails", db.Namespace, name)
	}
	return fmt.Errorf("ObjectStore %s/%s had no endpointCA — it predates the cnpg-obj-proxy-ca policy — and now carries it, but the "+
		"running instances were built without the CA. Restart them (`kubectl cnpg restart %s -n %s`, a rolling restart) and re-run",
		db.Namespace, name, db.Cluster, db.Namespace)
}

func waitArchiving(ctx context.Context, db clusterspec.CnpgDatabase) error {
	var why string
	err := poll(ctx, archiveWait, func() (bool, error) {
		out, err := getJSON(db.Namespace, clusterKind, db.Cluster)
		if err != nil {
			why = err.Error()
			return false, nil
		}
		done, w := ArchivingVerdict(out)
		why = w
		return done, nil
	})
	if err != nil {
		return fmt.Errorf("continuous WAL archiving is not working on %s/%s after %s: %s%s", db.Namespace, db.Cluster, archiveWait, why, archiveHint(why))
	}
	fmt.Printf("✓ %s/%s: continuous WAL archiving is working\n", db.Namespace, db.Cluster)
	return nil
}

func takeBackup(ctx context.Context, db clusterspec.CnpgDatabase, name string) (BackupStatus, error) {
	if out, err := caps.Writer.ApplyStdin(string(BackupJSON(db, name)), createdBy); err != nil {
		return BackupStatus{}, fmt.Errorf("create Backup %s: %w\n%s", name, err, out)
	}
	fmt.Printf("created Backup %s/%s — waiting up to %s…\n", db.Namespace, name, backupWait)
	var st BackupStatus
	err := poll(ctx, backupWait, func() (bool, error) {
		out, err := getJSON(db.Namespace, backupKind, name)
		if err != nil {
			return false, nil
		}
		var b struct {
			Status BackupStatus `json:"status"`
		}
		if err := json.Unmarshal(out, &b); err != nil {
			return false, nil
		}
		st = b.Status
		return st.Phase == "completed" || st.Phase == "failed", nil
	})
	if err != nil {
		return st, fmt.Errorf("backup %s did not finish within %s (last phase %s, error %s)", name, backupWait, orUnknown(st.Phase), orUnknown(st.Error))
	}
	return st, nil
}

func verifyInBucket(ctx context.Context, st baosnap.Store, tg target, d Destination, serverName string, b BackupStatus) error {
	base := basePrefix(d, serverName, b.BackupID)
	baseObjs, err := st.List(ctx, base)
	if err != nil {
		return fmt.Errorf("list %s/%s (could not tell whether the backup arrived): %w", tg.bucket, base, err)
	}
	walDir := walPrefix(d, serverName, b.EndWal)
	walObjs, err := st.List(ctx, walDir)
	if err != nil {
		return fmt.Errorf("list %s/%s (could not tell whether the WAL arrived): %w", tg.bucket, walDir, err)
	}
	if err := BucketVerdict(d, serverName, b, keysOf(baseObjs), keysOf(walObjs)); err != nil {
		return err
	}
	info := base + "backup.info"
	code, s3code, err := st.GetStatus(ctx, info)
	if err != nil {
		return fmt.Errorf("probe %s/%s: %w", tg.bucket, info, err)
	}
	if err := EncryptionVerdict(tg.bucket+"/"+info, tg.objProxy, code, s3code); err != nil {
		return err
	}
	mode := "plaintext at rest (objProxy is off — see the cnpg bucket's at-rest registration)"
	if tg.objProxy {
		mode = "SSE-C through objProxy (a keyless read is refused)"
	}
	fmt.Printf("✓ %s holds base backup %s and end WAL %s where a recovery looks; %s\n", tg.bucket, base, b.EndWal, mode)
	return nil
}

func keysOf(objs []baosnap.Object) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.Key)
	}
	return out
}

// poll calls fn every pollInterval until it reports done, fn errors, the budget
// runs out (an error) or ctx ends.
func poll(ctx context.Context, budget time.Duration, fn func() (bool, error)) error {
	deadline := time.Now().Add(budget)
	for {
		done, err := fn()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", budget)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none reported)"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
