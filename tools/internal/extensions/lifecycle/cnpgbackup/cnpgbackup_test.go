package cnpgbackup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// ── a small fake cluster that behaves like apl-core + CNPG where the gate looks ──

type fakeCNPG struct {
	mu   sync.Mutex
	objs map[string]map[string]any // kind/ns/name → object

	// liveDBs are the databases each live cluster holds; a Backup snapshots them,
	// and a recovery from that backup holds exactly the snapshot.
	liveDBs map[string][]string
	snaps   map[string][]string // backupId → databases

	// knobs for the failure arms
	unwired       bool   // the Cluster carries no plugin stanza
	inTree        bool   // the Cluster uses barmanObjectStore
	archiving     string // ContinuousArchiving status; "" = True
	archivingMsg  string
	backupPhase   string // "" = completed
	noBaseObjects bool   // the backup "completes" but nothing lands
	noWAL         bool   // the base lands but not its end WAL
	keylessStatus int    // keyless GET status; 0 = 200 (plaintext)
	wrongBucket   string // the ObjectStore names another bucket
	noCA          bool   // the ObjectStore lacks endpointCA
	kyvernoFixes  bool   // re-admission adds endpointCA
	restoreEmpty  bool   // the recovery comes up without the canary

	uploaded       map[string]bool
	backupsApplied []string
	annotated      []string
}

func k(kind, ns, name string) string { return strings.ToLower(kind) + "/" + ns + "/" + name }

var kinds = map[string]string{
	clusterKind: "cluster", backupKind: "backup", databaseKind: "database", objectStoreKind: "objectstore",
	"jobs.batch": "job", "namespace": "namespace", "pv": "persistentvolume",
}

func newFake(bucket string) *fakeCNPG {
	f := &fakeCNPG{objs: map[string]map[string]any{}, liveDBs: map[string][]string{}, snaps: map[string][]string{},
		uploaded: map[string]bool{}, kyvernoFixes: true}
	for _, db := range clusterspec.CnpgDatabases {
		f.liveDBs[db.Cluster] = []string{db.Database}
		f.objs[k("objectstore", db.Namespace, db.Cluster)] = map[string]any{
			"spec": map[string]any{"configuration": map[string]any{
				"destinationPath": "s3://" + bucket + "/" + db.Key + "-1",
				"endpointURL":     "https://us-ord-10.linodeobjects.com",
				"endpointCA":      map[string]any{"name": "obj-proxy-ca", "key": "ca.crt"},
			}, "instanceSidecarConfiguration": map[string]any{"env": []any{
				map[string]any{"name": "AWS_REQUEST_CHECKSUM_CALCULATION", "value": "WHEN_REQUIRED"}}}},
		}
	}
	return f
}

func (f *fakeCNPG) cluster(db clusterspec.CnpgDatabase) map[string]any {
	spec := map[string]any{"imageName": "ghcr.io/cloudnative-pg/postgresql:15.18-standard-trixie"}
	switch {
	case f.inTree:
		spec["backup"] = map[string]any{"barmanObjectStore": map[string]any{"destinationPath": "s3://x/y"}}
	case !f.unwired:
		spec["plugins"] = []any{map[string]any{"name": clusterspec.CnpgBarmanPlugin, "isWALArchiver": true,
			"parameters": map[string]any{"barmanObjectName": db.Cluster}}}
	}
	st := f.archiving
	if st == "" {
		st = "True"
	}
	return map[string]any{"metadata": map[string]any{"name": db.Cluster}, "spec": spec,
		"status": map[string]any{"conditions": []any{map[string]any{"type": "ContinuousArchiving", "status": st, "message": f.archivingMsg}}}}
}

func (f *fakeCNPG) Run(args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ns := ""
	if len(args) > 1 && args[0] == "-n" {
		ns, args = args[1], args[2:]
	}
	if len(args) >= 2 && args[0] == "logs" {
		job := k("job", ns, strings.TrimPrefix(args[1], "job/"))
		if o, ok := f.objs[job]; ok {
			return []byte(o["logs"].(string)), nil
		}
		return nil, fmt.Errorf("no job")
	}
	if len(args) < 2 || args[0] != "get" {
		return nil, fmt.Errorf("fake: unsupported %v", args)
	}
	kind := kinds[args[1]]
	if kind == "persistentvolume" {
		var items []any
		for kk, o := range f.objs {
			if strings.HasPrefix(kk, "persistentvolume/") {
				items = append(items, o)
			}
		}
		return json.Marshal(map[string]any{"items": items})
	}
	name := args[2]
	if kind == "namespace" {
		if _, ok := f.objs[k("namespace", "", name)]; !ok {
			return []byte(""), nil
		}
		return []byte("namespace/" + name), nil
	}
	if kind == "cluster" {
		for _, db := range clusterspec.CnpgDatabases {
			if db.Namespace == ns && db.Cluster == name {
				return json.Marshal(f.cluster(db))
			}
		}
	}
	o, ok := f.objs[k(kind, ns, name)]
	if !ok {
		return nil, fmt.Errorf("Error from server (NotFound): %s %q not found", kind, name)
	}
	if kind == "objectstore" {
		c := deepCopy(o)
		cfg := c["spec"].(map[string]any)["configuration"].(map[string]any)
		if f.wrongBucket != "" {
			cfg["destinationPath"] = "s3://" + f.wrongBucket + "/x-1"
		}
		if f.noCA && ns != "" && !strings.HasPrefix(ns, "llz-cnpg-drill") {
			delete(cfg, "endpointCA")
		}
		return json.Marshal(c)
	}
	return json.Marshal(o)
}
func (f *fakeCNPG) Combined(args ...string) string { b, _ := f.Run(args...); return string(b) }
func (f *fakeCNPG) Permits(...string) error        { return nil }

func deepCopy(o map[string]any) map[string]any {
	b, _ := json.Marshal(o)
	var c map[string]any
	_ = json.Unmarshal(b, &c)
	return c
}

func meta(o map[string]any) (ns, name string) {
	m := o["metadata"].(map[string]any)
	ns, _ = m["namespace"].(string)
	name, _ = m["name"].(string)
	return ns, name
}

// ApplyStdin is where the fake plays CNPG and the plugin.
func (f *fakeCNPG) ApplyStdin(manifest, _ string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var o map[string]any
	if err := json.Unmarshal([]byte(manifest), &o); err != nil {
		return nil, err
	}
	items := []map[string]any{o}
	if o["kind"] == "List" {
		items = nil
		for _, it := range o["items"].([]any) {
			items = append(items, it.(map[string]any))
		}
	}
	for _, it := range items {
		f.applyOne(it)
	}
	return []byte("applied"), nil
}

func (f *fakeCNPG) applyOne(o map[string]any) {
	kind := strings.ToLower(o["kind"].(string))
	ns, name := meta(o)
	spec, _ := o["spec"].(map[string]any)
	switch kind {
	case "database":
		cl := spec["cluster"].(map[string]any)["name"].(string)
		f.liveDBs[cl] = append(f.liveDBs[cl], spec["name"].(string))
		o["status"] = map[string]any{"applied": true}
	case "backup":
		f.backupsApplied = append(f.backupsApplied, name)
		cl := spec["cluster"].(map[string]any)["name"].(string)
		id := fmt.Sprintf("2026101%dT000700", len(f.backupsApplied))
		f.snaps[id] = append([]string(nil), f.liveDBs[cl]...)
		phase := f.backupPhase
		if phase == "" {
			phase = "completed"
		}
		end := "000000010000000000000005"
		o["status"] = map[string]any{"phase": phase, "backupId": id, "beginWal": "000000010000000000000004", "endWal": end, "error": "fake failure"}
		var prefix string
		for _, db := range clusterspec.CnpgDatabases {
			if db.Cluster == cl {
				prefix = db.Key + "-1/" + cl
			}
		}
		if !f.noBaseObjects {
			f.uploaded[prefix+"/base/"+id+"/backup.info"] = true
			f.uploaded[prefix+"/base/"+id+"/data.tar.gz"] = true
		}
		if !f.noWAL {
			f.uploaded[prefix+"/wals/0000000100000000/"+end+".gz"] = true
		}
	case "cluster":
		// The drill's recovery cluster.
		id := spec["bootstrap"].(map[string]any)["recovery"].(map[string]any)["recoveryTarget"].(map[string]any)["backupID"].(string)
		dbs := f.snaps[id]
		if f.restoreEmpty {
			dbs = nil
		}
		restored := make([]any, len(dbs))
		for i, d := range dbs {
			restored[i] = d
		}
		o["restored"] = restored
		o["status"] = map[string]any{"phase": "Cluster in healthy state", "readyInstances": 1}
	case "job":
		cl := f.objs[k("cluster", ns, drillName)]
		cmd := spec["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["command"].([]any)
		q := cmd[len(cmd)-1].(string)
		var out string
		if cl != nil {
			for _, d := range cl["restored"].([]any) {
				if strings.Contains(q, "'"+d.(string)+"'") {
					out = d.(string) + "\n"
				}
			}
		}
		o["logs"] = out
		o["status"] = map[string]any{"succeeded": 1}
	case "namespace":
		ns = ""
	}
	f.objs[k(kind, ns, name)] = o
}

func (f *fakeCNPG) Delete(ns, kind string, target ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kd := kinds[kind]
	name := target[0]
	if kd == "namespace" {
		for kk := range f.objs {
			if strings.Split(kk, "/")[1] == name || kk == k("namespace", "", name) {
				delete(f.objs, kk)
			}
		}
		return nil, nil
	}
	if kd == "database" {
		o := f.objs[k("database", ns, name)]
		if o != nil {
			spec := o["spec"].(map[string]any)
			cl := spec["cluster"].(map[string]any)["name"].(string)
			var keep []string
			for _, d := range f.liveDBs[cl] {
				if d != spec["name"] {
					keep = append(keep, d)
				}
			}
			f.liveDBs[cl] = keep
		}
	}
	delete(f.objs, k(kd, ns, name))
	return nil, nil
}
func (f *fakeCNPG) Annotate(ns, kind, name, kv string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.annotated = append(f.annotated, ns+"/"+name)
	if f.kyvernoFixes {
		f.noCA = false
	}
	return nil, nil
}
func (f *fakeCNPG) DeleteOrphan(string, string, string) ([]byte, error)       { return nil, nil }
func (f *fakeCNPG) PatchMerge(string, string, string, string) ([]byte, error) { return nil, nil }
func (f *fakeCNPG) RolloutRestart(string, string) ([]byte, error)             { return nil, nil }
func (f *fakeCNPG) CreateToken(string, string, string) ([]byte, error)        { return nil, nil }
func (f *fakeCNPG) ApplyServerSide(string, string) ([]byte, error)            { return nil, nil }
func (f *fakeCNPG) CreateStdin(string, string) ([]byte, error)                { return nil, nil }
func (f *fakeCNPG) PermitsWrite() error                                       { return nil }

// ServeHTTP is the cnpg bucket: LIST under a prefix, and a keyless GET.
func (f *fakeCNPG) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	if q.Get("list-type") == "2" {
		var keys []string
		for kk := range f.uploaded {
			if strings.HasPrefix(kk, q.Get("prefix")) {
				keys = append(keys, kk)
			}
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("<ListBucketResult>")
		for _, kk := range keys {
			fmt.Fprintf(&b, "<Contents><Key>%s</Key><Size>1</Size><LastModified>2026-10-15T00:00:00Z</LastModified></Contents>", kk)
		}
		b.WriteString("<IsTruncated>false</IsTruncated></ListBucketResult>")
		_, _ = w.Write([]byte(b.String()))
		return
	}
	if f.keylessStatus != 0 {
		w.WriteHeader(f.keylessStatus)
		_, _ = w.Write([]byte("<Error><Code>InvalidRequest</Code></Error>"))
		return
	}
	_, _ = w.Write([]byte("backup.info plaintext"))
}

// harness: a spec with cnpgBackup on for e2e and prod (objProxy as given), the
// fake as cluster + writer + bucket, and a recorded scrape call.
func harness(t *testing.T, f *fakeCNPG, enabled, objProxy bool, extra string) *[][]string {
	t.Helper()
	root := t.TempDir()
	lz := "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata:\n  name: inst\nspec:\n  instance:\n    upstreamOrg: akamai-consulting\n    repo: acme/inst\n    forge: github\n"
	for _, env := range []string{"e2e", "prod"} {
		body := fmt.Sprintf("apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: ClusterDefinition\nmetadata:\n  name: %s\nspec:\n  cluster:\n    region: us-ord\n    objectStorage:\n      cluster: us-ord-10\n  components:\n    cnpgBackup: {enabled: %v%s}\n    objProxy: {enabled: %v}\n", env, enabled, extra, objProxy)
		if err := os.MkdirAll(filepath.Join(root, "environments"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "environments", env+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "landingzone.yaml"), []byte(lz), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("LINODE_API_TOKEN", "lin")

	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	scrapes := &[][]string{}
	prev := caps
	caps = Deps{
		Cluster: f, Writer: f,
		Keys: func(string) baosnap.KeyAPI { return nil },
		TempStore: func(_ context.Context, _ baosnap.KeyAPI, _, _, bucket, _, perms string) (baosnap.Store, func(), error) {
			if perms != "read_only" {
				t.Errorf("the check minted a %s key; it only reads", perms)
			}
			if bucket != "inst-cnpg-e2e" && bucket != "inst-cnpg-prod" {
				t.Errorf("the check read %q, not the spec-derived cnpg bucket", bucket)
			}
			return baosnap.Store{AccessKey: "AK", SecretKey: "SK", Endpoint: srv.URL, Bucket: bucket, HTTP: srv.Client()}, func() {}, nil
		},
		Scrape: func(args ...string) error { *scrapes = append(*scrapes, args); return nil },
	}
	pi, pn := pollInterval, now
	pollInterval = time.Millisecond
	now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	for _, b := range []*time.Duration{&wireWait, &archiveWait, &backupWait, &canaryWait, &restoreWait, &verifyWait, &nsGoneWait} {
		old := *b
		*b = 50 * time.Millisecond
		t.Cleanup(func() { *b = old })
	}
	t.Cleanup(func() { caps, pollInterval, now = prev, pi, pn })
	return scrapes
}

func TestCheckProvesEachDatabaseBacksUpWhereARestoreLooks(t *testing.T) {
	f := newFake("inst-cnpg-prod")
	scrapes := harness(t, f, true, false, "")
	if err := Run(context.Background(), "prod", false); err != nil {
		t.Fatal(err)
	}
	// Both databases (harbor runs by default), one on-demand Backup each, by the
	// plugin.
	if len(f.backupsApplied) != 2 {
		t.Fatalf("backups = %v, want one per database", f.backupsApplied)
	}
	for _, db := range clusterspec.CnpgDatabases {
		var found map[string]any
		for _, n := range f.backupsApplied {
			if o := f.objs[k("backup", db.Namespace, n)]; o != nil {
				found = o
			}
		}
		if found == nil {
			t.Errorf("no Backup in %s", db.Namespace)
			continue
		}
		spec := found["spec"].(map[string]any)
		if spec["method"] != "plugin" || spec["pluginConfiguration"].(map[string]any)["name"] != clusterspec.CnpgBarmanPlugin {
			t.Errorf("%s backup spec = %v, want the plugin method the ScheduledBackup uses", db.Key, spec)
		}
	}
	if len(*scrapes) != 1 || strings.Join((*scrapes)[0], " ") != "--monitors= --rule-groups llz-cnpg-backup" {
		t.Errorf("scrape calls = %v", *scrapes)
	}
	for kk := range f.objs {
		if strings.HasPrefix(kk, "database/") || strings.Contains(kk, "llz-cnpg-drill") {
			t.Errorf("the check (no --drill) created %s", kk)
		}
	}
}

// Fail closed, each arm on its own.
func TestCheckFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		mut      func(*fakeCNPG)
		objProxy bool
		want     string
	}{
		"overlay never delivered": {func(f *fakeCNPG) { f.unwired = true }, false, "never wired for backups"},
		"in-tree barman":          {func(f *fakeCNPG) { f.inTree = true }, false, "IN-TREE barmanObjectStore"},
		"wrong bucket":            {func(f *fakeCNPG) { f.wrongBucket = "someone-elses" }, false, `bucket "someone-elses"`},
		"archiving failing": {func(f *fakeCNPG) {
			f.archiving, f.archivingMsg = "False", "Expected empty archive"
		}, false, "ALREADY HOLDS an archive"},
		"backup failed":           {func(f *fakeCNPG) { f.backupPhase = "failed" }, false, "ended failed"},
		"completed, nothing sent": {func(f *fakeCNPG) { f.noBaseObjects = true }, false, "no complete base backup"},
		"no end WAL":              {func(f *fakeCNPG) { f.noWAL = true }, false, "end WAL segment"},
		"plaintext under objProxy": {func(f *fakeCNPG) {
			f.keylessStatus = 0
		}, true, "READ BACK WITHOUT AN SSE-C KEY"},
		"encrypted without objProxy": {func(f *fakeCNPG) { f.keylessStatus = 400 }, false, "although objProxy is off"},
		"could not tell":             {func(f *fakeCNPG) { f.keylessStatus = 403 }, false, "could not tell"},
		"no CA, kyverno fixes it": {func(f *fakeCNPG) {
			f.noCA = true
		}, true, "Restart them"},
		"no CA, kyverno does not": {func(f *fakeCNPG) {
			f.noCA, f.kyvernoFixes = true, false
		}, true, "did not mutate"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake("inst-cnpg-prod")
			if tc.objProxy {
				f.keylessStatus = 400
			}
			tc.mut(f)
			harness(t, f, true, tc.objProxy, "")
			err := Run(context.Background(), "prod", false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The happy path under objProxy: the keyless probe is refused, and the CA is
// already on the store, so nothing is re-admitted.
func TestCheckUnderObjProxyRequiresCiphertext(t *testing.T) {
	f := newFake("inst-cnpg-prod")
	f.keylessStatus = 400
	harness(t, f, true, true, "")
	if err := Run(context.Background(), "prod", false); err != nil {
		t.Fatal(err)
	}
	if len(f.annotated) != 0 {
		t.Errorf("an ObjectStore that already trusts the proxy was re-admitted: %v", f.annotated)
	}
}

func TestDrillRestoresTheCanary(t *testing.T) {
	f := newFake("inst-cnpg-e2e")
	harness(t, f, true, false, ", backupDatabases: keycloak")
	if err := Run(context.Background(), "e2e", true); err != nil {
		t.Fatal(err)
	}
	if len(f.backupsApplied) != 1 {
		t.Fatalf("backups = %v, want one (the check's, taken after the canary)", f.backupsApplied)
	}
	// Cleanup: the canary is dropped from the live cluster, the scratch namespace
	// is gone.
	for _, d := range f.liveDBs["keycloak-db"] {
		if strings.HasPrefix(d, "llz_canary_") {
			t.Errorf("the canary database %s was left in the live cluster", d)
		}
	}
	for kk := range f.objs {
		if strings.Contains(kk, "llz-cnpg-drill-keycloak") {
			t.Errorf("the drill left %s behind", kk)
		}
	}
}

// A recovery that comes up healthy WITHOUT the canary — an earlier backup, an
// empty initdb — must fail: that is the whole point of writing it first.
func TestDrillFailsWhenTheRestoreLacksTheCanary(t *testing.T) {
	f := newFake("inst-cnpg-e2e")
	f.restoreEmpty = true
	harness(t, f, true, false, ", backupDatabases: keycloak")
	err := Run(context.Background(), "e2e", true)
	if err == nil || !strings.Contains(err.Error(), "has no database llz_canary_") {
		t.Errorf("err = %v", err)
	}
}

func TestDrillRefusesRealDeploymentsAndSkipsWhenDisabled(t *testing.T) {
	f := newFake("inst-cnpg-prod")
	harness(t, f, true, false, "")
	if err := Run(context.Background(), "prod", true); err == nil || !strings.Contains(err.Error(), "not an e2e lane") {
		t.Errorf("--drill on prod = %v", err)
	}
	if len(f.backupsApplied) != 0 {
		t.Error("a refused drill still took a backup")
	}
	f2 := newFake("inst-cnpg-e2e")
	harness(t, f2, false, false, "")
	if err := Run(context.Background(), "e2e", true); err != nil {
		t.Errorf("disabled component: %v, want a clean skip", err)
	}
	if len(f2.backupsApplied) != 0 {
		t.Error("a disabled component was backed up")
	}
}

// The scratch recovery reads, and can only read: the drill's ObjectStore points
// at the LIVE destination under the temporary read-only key, the scratch Cluster
// has no WAL archiver, and recovery stops at exactly the checked backup.
func TestDrillManifestCannotWriteTheArchive(t *testing.T) {
	db, _ := clusterspec.LookupCnpgDatabase("keycloak")
	d := Destination{Path: "s3://inst-cnpg-e2e/keycloak-1", Bucket: "inst-cnpg-e2e", Prefix: "keycloak-1", EndpointURL: "https://us-ord-10.linodeobjects.com"}
	m := DrillManifest(db, Wiring{ObjectStore: "keycloak-db", ServerName: "keycloak-db", ImageName: "img"}, d, BackupStatus{BackupID: "B1"}, "AK", "SK", true)
	var l struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(m), &l); err != nil {
		t.Fatal(err)
	}
	var sawStore, sawCluster, sawCA bool
	for _, it := range l.Items {
		switch it["kind"] {
		case "ObjectStore":
			sawStore = true
			cfg := it["spec"].(map[string]any)["configuration"].(map[string]any)
			if cfg["destinationPath"] != d.Path || cfg["endpointCA"] == nil {
				t.Errorf("drill store = %v", cfg)
			}
		case "Cluster":
			sawCluster = true
			spec := it["spec"].(map[string]any)
			if _, ok := spec["plugins"]; ok {
				t.Error("the scratch cluster has a plugins stanza — it could archive into the live destination")
			}
			rt := spec["bootstrap"].(map[string]any)["recovery"].(map[string]any)["recoveryTarget"].(map[string]any)
			if rt["backupID"] != "B1" {
				t.Errorf("recovery target = %v, want the checked backup", rt)
			}
			ext := spec["externalClusters"].([]any)[0].(map[string]any)["plugin"].(map[string]any)["parameters"].(map[string]any)
			if ext["serverName"] != "keycloak-db" {
				t.Errorf("recovery reads serverName %v, not the live cluster's", ext["serverName"])
			}
		case "Certificate":
			sawCA = true
		}
	}
	if !sawStore || !sawCluster || !sawCA {
		t.Errorf("manifest kinds: store=%v cluster=%v ca=%v", sawStore, sawCluster, sawCA)
	}
}

func TestPureVerdicts(t *testing.T) {
	d := Destination{Bucket: "b", Prefix: "keycloak-1", EndpointURL: "https://us-ord-10.linodeobjects.com"}
	if err := DestinationVerdict(d, "b", "us-ord-10.linodeobjects.com"); err != nil {
		t.Error(err)
	}
	if DestinationVerdict(d, "b", "us-sea-1.linodeobjects.com") == nil {
		t.Error("a store on another endpoint passed")
	}
	if DestinationVerdict(Destination{Bucket: "b", EndpointURL: d.EndpointURL}, "b", "us-ord-10.linodeobjects.com") == nil {
		t.Error("a destination with no pathSuffix passed")
	}
	if _, err := ParseObjectStore([]byte(`{"spec":{"configuration":{"destinationPath":"/local/path"}}}`)); err == nil {
		t.Error("a non-s3 destination parsed")
	}
	if _, _, err := ParseWiring([]byte(`{"spec":{}}`)); err == nil {
		t.Error("a Cluster with no name was taken as read from the cluster")
	}
	st := BackupStatus{Phase: "completed", BackupID: "B", EndWal: "000000010000000000000005"}
	db, _ := clusterspec.LookupCnpgDatabase("keycloak")
	if err := BackupVerdict(db, "n", st); err != nil {
		t.Error(err)
	}
	if BackupVerdict(db, "n", BackupStatus{Phase: "completed"}) == nil {
		t.Error("a completed backup with no id passed — it cannot be located")
	}
	base := "keycloak-1/keycloak-db/base/B/"
	wal := "keycloak-1/keycloak-db/wals/0000000100000000/000000010000000000000005.gz"
	if err := BucketVerdict(d, "keycloak-db", st, []string{base + "backup.info", base + "data.tar.gz"}, []string{wal}); err != nil {
		t.Error(err)
	}
	if BucketVerdict(d, "keycloak-db", st, []string{base + "backup.info"}, []string{wal}) == nil {
		t.Error("a base backup with no data passed")
	}
	if BucketVerdict(d, "other", st, []string{base + "backup.info", base + "data.tar.gz"}, []string{wal}) == nil {
		t.Error("objects under another serverName passed — a recovery would not look there")
	}
	if done, _ := ArchivingVerdict([]byte(`{"status":{}}`)); done {
		t.Error("no condition read as archiving")
	}
	if CanaryVerdict("llz_canary_x\n", "llz_canary_x") != nil || CanaryVerdict("", "llz_canary_x") == nil ||
		CanaryVerdict("llz_canary_xy\n", "llz_canary_x") == nil {
		t.Error("canary verdict")
	}
	pvs := ReleasedPVsOf([]byte(`{"items":[{"metadata":{"name":"a"},"spec":{"claimRef":{"namespace":"llz-cnpg-drill-keycloak"}}},
		{"metadata":{"name":"b"},"spec":{"claimRef":{"namespace":"keycloak"}}},{"metadata":{"name":"c"},"spec":{}}]}`), "llz-cnpg-drill-keycloak")
	if strings.Join(pvs, ",") != "a" {
		t.Errorf("released PVs = %v — the drill must only ever delete its own", pvs)
	}
}

// The gate and the alerts must name the same plugin metric family the rules
// file reads: a rename on one side is the split contract e2e-gates.md warns of.
func TestAlertsReadThePluginMetrics(t *testing.T) {
	raw, err := os.ReadFile("../../../../../platform-apl/components/cnpgBackup/cnpg-backup/prometheusrule.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{"- name: " + clusterspec.CnpgBackupRuleGroup, "barman_cloud_cloudnative_pg_io_last_available_backup_timestamp",
		"absent(barman_cloud_cloudnative_pg_io_last_available_backup_timestamp", "namespace: " + clusterspec.CnpgBackupNamespace} {
		if !strings.Contains(s, want) {
			t.Errorf("prometheusrule.yaml lacks %q", want)
		}
	}
	// Every namespace the rules select is a database namespace the registry knows,
	// and every database namespace is selected.
	for _, db := range clusterspec.CnpgDatabases {
		if !strings.Contains(s, db.Namespace) {
			t.Errorf("the rules never select %s", db.Namespace)
		}
	}
	if !strings.Contains(s, `namespace=~"harbor|keycloak"`) {
		t.Error("the rules' namespace selector moved; update this test with the database list")
	}
}
