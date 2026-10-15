package velero

import (
	"context"
	"encoding/base64"
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

// ── a small fake cluster that behaves like Velero where the drill looks ───────

type fakeKube struct {
	mu   sync.Mutex
	objs map[string]map[string]any // kind/ns/name → object
	// snaps are what each Backup captured: the objects of its included namespaces.
	snaps map[string][]map[string]any
	// knobs for the failure arms
	itemsOverride  *int   // the check backup's itemsBackedUp, when set
	backupPhase    string // "" = Completed
	restoreNoop    bool   // a Restore that Completes having recreated nothing
	encrypted      bool   // the bucket refuses keyless reads
	bslPhase       string // "" = Available
	uploaded       map[string]bool
	backupsApplied []string
}

func key(kind, ns, name string) string { return strings.ToLower(kind) + "/" + ns + "/" + name }

func newFakeKube(t *testing.T, excludes []string) *fakeKube {
	t.Helper()
	k := &fakeKube{objs: map[string]map[string]any{}, snaps: map[string][]map[string]any{}, encrypted: true, uploaded: map[string]bool{}}
	k.objs[key("schedule", clusterspec.VeleroNamespace, clusterspec.VeleroScheduleName)] = map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Schedule",
		"metadata": map[string]any{"name": clusterspec.VeleroScheduleName, "namespace": clusterspec.VeleroNamespace},
		"spec": map[string]any{"schedule": "37 1 * * *", "template": map[string]any{
			"ttl": "720h", "storageLocation": "default", "snapshotVolumes": false,
			"includedNamespaces": []any{"*"}, "excludedNamespaces": toAny(excludes),
		}},
	}
	return k
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// kindOf maps the drill's kubectl resource names to the fake's kinds.
func kindOf(r string) string {
	switch r {
	case "schedules.velero.io":
		return "schedule"
	case "backupstoragelocations.velero.io":
		return "backupstoragelocation"
	case "backups.velero.io":
		return "backup"
	case "restores.velero.io":
		return "restore"
	}
	return r
}

func (k *fakeKube) Run(args ...string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	ns := ""
	if len(args) > 1 && args[0] == "-n" {
		ns, args = args[1], args[2:]
	}
	if len(args) < 3 || args[0] != "get" {
		return nil, fmt.Errorf("fake: unsupported %v", args)
	}
	kind, name := kindOf(args[1]), args[2]
	if kind == "backupstoragelocation" {
		phase := k.bslPhase
		if phase == "" {
			phase = "Available"
		}
		return json.Marshal(map[string]any{"status": map[string]any{"phase": phase, "message": "fake"}})
	}
	o, ok := k.objs[key(kind, ns, name)]
	if kind == "namespace" {
		if !ok {
			return []byte(""), nil // --ignore-not-found -o name
		}
		return []byte("namespace/" + name), nil
	}
	if !ok {
		return nil, fmt.Errorf("Error from server (NotFound): %s %q not found", kind, name)
	}
	return json.Marshal(o)
}
func (k *fakeKube) Combined(args ...string) string { b, _ := k.Run(args...); return string(b) }
func (k *fakeKube) Permits(...string) error        { return nil }

// ApplyStdin is where the fake plays Velero: a Backup snapshots its included
// namespaces and "uploads"; a Restore recreates a snapshot with Velero's label.
func (k *fakeKube) ApplyStdin(manifest, _ string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
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
		k.applyOne(it)
	}
	return []byte("applied"), nil
}

func meta(o map[string]any) (ns, name string) {
	m := o["metadata"].(map[string]any)
	ns, _ = m["namespace"].(string)
	name, _ = m["name"].(string)
	return ns, name
}

func (k *fakeKube) applyOne(o map[string]any) {
	kind := strings.ToLower(o["kind"].(string))
	ns, name := meta(o)
	if kind == "secret" {
		// The API server turns stringData into base64 data.
		if sd, ok := o["stringData"].(map[string]any); ok {
			d := map[string]any{}
			for kk, v := range sd {
				d[kk] = base64.StdEncoding.EncodeToString([]byte(v.(string)))
			}
			o["data"] = d
			delete(o, "stringData")
		}
	}
	switch kind {
	case "backup":
		k.backupsApplied = append(k.backupsApplied, name)
		spec := o["spec"].(map[string]any)
		var snap []map[string]any
		inc, _ := spec["includedNamespaces"].([]any)
		if len(inc) == 1 && inc[0] != "*" {
			for kk, v := range k.objs {
				if strings.Split(kk, "/")[1] == inc[0] || kk == key("namespace", "", inc[0].(string)) {
					snap = append(snap, deepCopy(v))
				}
			}
		}
		k.snaps[name] = snap
		items := len(snap)
		if inc == nil || (len(inc) == 1 && inc[0] == "*") {
			items = 42
		}
		if k.itemsOverride != nil {
			items = *k.itemsOverride
		}
		phase := k.backupPhase
		if phase == "" {
			phase = "Completed"
		}
		o["status"] = map[string]any{"phase": phase, "progress": map[string]any{"totalItems": items, "itemsBackedUp": items}}
		k.uploaded["backups/"+name+"/"+name+".tar.gz"] = true
		k.uploaded["backups/"+name+"/velero-backup.json"] = true
	case "restore":
		spec := o["spec"].(map[string]any)
		snap := k.snaps[spec["backupName"].(string)]
		n := 0
		if !k.restoreNoop {
			for _, s := range snap {
				c := deepCopy(s)
				m := c["metadata"].(map[string]any)
				m["labels"] = map[string]any{"velero.io/restore-name": name}
				cns, cname := meta(c)
				k.objs[key(c["kind"].(string), cns, cname)] = c
				n++
			}
		}
		o["status"] = map[string]any{"phase": "Completed", "progress": map[string]any{"totalItems": n, "itemsRestored": max(n, 1)}}
	}
	k.objs[key(kind, ns, name)] = o
}

func deepCopy(o map[string]any) map[string]any {
	b, _ := json.Marshal(o)
	var c map[string]any
	_ = json.Unmarshal(b, &c)
	return c
}

func (k *fakeKube) Delete(ns, kind string, target ...string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	kind = kindOf(kind)
	name := target[0]
	if kind == "namespace" {
		for kk := range k.objs {
			if strings.Split(kk, "/")[1] == name || kk == key("namespace", "", name) {
				delete(k.objs, kk)
			}
		}
		return nil, nil
	}
	delete(k.objs, key(kind, ns, name))
	return nil, nil
}
func (k *fakeKube) Annotate(string, string, string, string) ([]byte, error) { return nil, nil }
func (k *fakeKube) DeleteOrphan(string, string, string) ([]byte, error)     { return nil, nil }
func (k *fakeKube) PatchMerge(string, string, string, string) ([]byte, error) {
	return nil, nil
}
func (k *fakeKube) RolloutRestart(string, string) ([]byte, error)      { return nil, nil }
func (k *fakeKube) CreateToken(string, string, string) ([]byte, error) { return nil, nil }
func (k *fakeKube) ApplyServerSide(string, string) ([]byte, error)     { return nil, nil }
func (k *fakeKube) CreateStdin(string, string) ([]byte, error)         { return nil, nil }
func (k *fakeKube) PermitsWrite() error                                { return nil }

// ServeHTTP is the velero bucket: LIST under a prefix, and a keyless GET that
// SSE-C objects refuse.
func (k *fakeKube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	q := r.URL.Query()
	if q.Get("list-type") == "2" {
		var keys []string
		for kk := range k.uploaded {
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
	if k.encrypted {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("<Error><Code>InvalidRequest</Code></Error>"))
		return
	}
	_, _ = w.Write([]byte("plaintext tarball"))
}

// harness: a spec with velero enabled for e2e, the fake as cluster + writer +
// bucket, and a recorded scrape call.
func harness(t *testing.T, k *fakeKube, enabled bool) *[][]string {
	t.Helper()
	root := t.TempDir()
	lz := "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata:\n  name: inst\nspec:\n  instance:\n    upstreamOrg: akamai-consulting\n    repo: acme/inst\n    forge: github\n"
	for _, env := range []string{"e2e", "prod"} {
		body := fmt.Sprintf("apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: ClusterDefinition\nmetadata:\n  name: %s\nspec:\n  cluster:\n    region: us-ord\n    objectStorage:\n      cluster: us-ord-10\n  components:\n    velero: {enabled: %v}\n", env, enabled)
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

	srv := httptest.NewServer(k)
	t.Cleanup(srv.Close)
	scrapes := &[][]string{}
	prev := caps
	caps = Deps{
		Cluster: k, Writer: k,
		Keys: func(string) baosnap.KeyAPI { return nil },
		TempStore: func(_ context.Context, _ baosnap.KeyAPI, _, _, bucket, _, perms string) (baosnap.Store, func(), error) {
			if perms != "read_only" {
				t.Errorf("the check minted a %s key; it only reads", perms)
			}
			if bucket != "inst-velero-e2e" && bucket != "inst-velero-prod" {
				t.Errorf("the check read %q, not the spec-derived velero bucket", bucket)
			}
			return baosnap.Store{AccessKey: "AK", SecretKey: "SK", Endpoint: srv.URL, Bucket: bucket, HTTP: srv.Client()}, func() {}, nil
		},
		Scrape: func(args ...string) error { *scrapes = append(*scrapes, args); return nil },
	}
	pi, ns := pollInterval, now
	pollInterval = time.Millisecond
	now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	for _, b := range []*time.Duration{&scheduleWait, &bslWait, &backupWait, &nsGoneWait, &restoreWait} {
		old := *b
		*b = 50 * time.Millisecond
		t.Cleanup(func() { *b = old })
	}
	t.Cleanup(func() { caps, pollInterval, now = prev, pi, ns })
	return scrapes
}

var safeExcludes = []string{"llz-openbao", "llz-velero", "kube-system"}

func TestBackupCheckProvesTheBackupLandsEncrypted(t *testing.T) {
	k := newFakeKube(t, safeExcludes)
	scrapes := harness(t, k, true)
	if err := Run(context.Background(), "prod", false); err != nil {
		t.Fatal(err)
	}
	if len(k.backupsApplied) != 1 || !strings.HasPrefix(k.backupsApplied[0], clusterspec.VeleroScheduleName+"-check-") {
		t.Fatalf("backups created = %v, want one check backup from the schedule", k.backupsApplied)
	}
	b := k.objs[key("backup", clusterspec.VeleroNamespace, k.backupsApplied[0])]
	labels := b["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels["velero.io/schedule-name"] != clusterspec.VeleroScheduleName {
		t.Errorf("the check backup is not labelled as the schedule's (%v) — the freshness alerts would not count it", labels)
	}
	if len(*scrapes) != 1 || strings.Join((*scrapes)[0], " ") != "--monitors llz-velero/velero --rule-groups llz-velero" {
		t.Errorf("scrape calls = %v", *scrapes)
	}
	if k.objs[key("namespace", "", DrillNamespace)] != nil {
		t.Error("the check (no --drill) created the canary namespace")
	}
}

// Fail closed, each arm on its own.
func TestBackupCheckFailsClosed(t *testing.T) {
	zero := 0
	for name, tc := range map[string]struct {
		mut  func(*fakeKube)
		want string
	}{
		"live schedule drops llz-openbao": {func(k *fakeKube) {
			*k = *newFakeKube(t, []string{"llz-velero"})
		}, "does not exclude llz-openbao"},
		"bsl unavailable":   {func(k *fakeKube) { k.bslPhase = "Unavailable" }, "not Available"},
		"backup failed":     {func(k *fakeKube) { k.backupPhase = "PartiallyFailed" }, "ended PartiallyFailed"},
		"empty backup":      {func(k *fakeKube) { k.itemsOverride = &zero }, "0 item(s)"},
		"plaintext objects": {func(k *fakeKube) { k.encrypted = false }, "WITHOUT ITS SSE-C KEY"},
		"no schedule": {func(k *fakeKube) {
			delete(k.objs, key("schedule", clusterspec.VeleroNamespace, clusterspec.VeleroScheduleName))
		}, "never appeared"},
	} {
		t.Run(name, func(t *testing.T) {
			k := newFakeKube(t, safeExcludes)
			tc.mut(k)
			harness(t, k, true)
			err := Run(context.Background(), "prod", false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
			if name == "live schedule drops llz-openbao" && len(k.backupsApplied) != 0 {
				t.Error("a backup was taken from an unsafe schedule")
			}
		})
	}
}

func TestDrillRoundTripsTheCanary(t *testing.T) {
	k := newFakeKube(t, safeExcludes)
	harness(t, k, true)
	if err := Run(context.Background(), "e2e", true); err != nil {
		t.Fatal(err)
	}
	if len(k.backupsApplied) != 2 {
		t.Fatalf("backups = %v, want the check backup and the drill backup", k.backupsApplied)
	}
	drill := k.objs[key("backup", clusterspec.VeleroNamespace, k.backupsApplied[1])]
	spec := drill["spec"].(map[string]any)
	if inc := spec["includedNamespaces"].([]any); len(inc) != 1 || inc[0] != DrillNamespace {
		t.Errorf("the drill backup is not narrowed to the canary namespace: %v", inc)
	}
	if ex := spec["excludedNamespaces"].([]any); len(ex) < 2 || ex[0] != "llz-openbao" {
		t.Errorf("the drill backup dropped the schedule's excludes: %v", ex)
	}
	if k.objs[key("namespace", "", DrillNamespace)] != nil {
		t.Error("the drill left its canary namespace behind")
	}
	if k.objs[key("deletebackuprequest", clusterspec.VeleroNamespace, k.backupsApplied[1]+"-delete")] == nil {
		t.Error("the drill did not request deletion of its own backup")
	}
}

// A restore that Completes having recreated nothing must FAIL the drill — that
// is the whole point of deleting the canary first.
func TestDrillFailsWhenTheRestoreBringsNothingBack(t *testing.T) {
	k := newFakeKube(t, safeExcludes)
	k.restoreNoop = true
	harness(t, k, true)
	err := Run(context.Background(), "e2e", true)
	if err == nil || !strings.Contains(err.Error(), "canary ConfigMap is not back") {
		t.Errorf("err = %v", err)
	}
}

func TestDrillRefusesRealDeploymentsAndSkipsWhenDisabled(t *testing.T) {
	k := newFakeKube(t, safeExcludes)
	harness(t, k, true)
	if err := Run(context.Background(), "prod", true); err == nil || !strings.Contains(err.Error(), "not an e2e lane") {
		t.Errorf("--drill on prod = %v", err)
	}
	k2 := newFakeKube(t, safeExcludes)
	harness(t, k2, false)
	if err := Run(context.Background(), "e2e", true); err != nil {
		t.Errorf("disabled component: %v, want a clean skip", err)
	}
	if len(k2.backupsApplied) != 0 {
		t.Error("a disabled component was backed up")
	}
}

func TestBackupFromScheduleKeepsTheSchedulesScope(t *testing.T) {
	sched := []byte(`{"metadata":{"name":"velero-cluster"},"spec":{"template":{"ttl":"720h","includedNamespaces":["*"],"excludedNamespaces":["llz-openbao","llz-velero"],"storageLocation":"default"}}}`)
	b, err := BackupFromSchedule(sched, "x", []string{DrillNamespace}, "1h", false)
	if err != nil {
		t.Fatal(err)
	}
	var o struct {
		Metadata struct {
			Namespace string            `json:"namespace"`
			Labels    map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec map[string]any `json:"spec"`
	}
	_ = json.Unmarshal(b, &o)
	if o.Metadata.Namespace != clusterspec.VeleroNamespace || o.Spec["ttl"] != "1h" || o.Spec["storageLocation"] != "default" {
		t.Errorf("backup = %s", b)
	}
	if _, ok := o.Metadata.Labels["velero.io/schedule-name"]; ok {
		t.Error("an ad-hoc drill backup carries the schedule label")
	}
	if _, err := BackupFromSchedule([]byte(`{"metadata":{"name":"s"},"spec":{}}`), "x", nil, "", true); err == nil {
		t.Error("a Schedule with no template was accepted")
	}
	if _, err := ScheduleScope([]byte(`{"spec":{}}`)); err == nil {
		t.Error("a Schedule with no template read as a scope")
	}
}

func TestRestoreIsScopedToTheCanary(t *testing.T) {
	var r struct {
		Spec struct {
			IncludedNamespaces     []string `json:"includedNamespaces"`
			ExistingResourcePolicy string   `json:"existingResourcePolicy"`
		} `json:"spec"`
	}
	_ = json.Unmarshal(RestoreJSON("r", "b"), &r)
	if strings.Join(r.Spec.IncludedNamespaces, ",") != DrillNamespace || r.Spec.ExistingResourcePolicy != "none" {
		t.Errorf("restore spec = %+v — the drill must never restore outside its canary or over an existing object", r.Spec)
	}
}

func TestVerdicts(t *testing.T) {
	ok := BackupStatus{Phase: "Completed"}
	ok.Progress.ItemsBackedUp = 3
	if err := BackupVerdict("b", ok, 3); err != nil {
		t.Error(err)
	}
	bad := ok
	bad.Errors = 1
	if BackupVerdict("b", bad, 1) == nil {
		t.Error("errors accepted")
	}
	if RestoreVerdict("r", RestoreStatus{Phase: "Completed"}) == nil {
		t.Error("a restore of nothing accepted")
	}
	if RestoreVerdict("r", RestoreStatus{Phase: "PartiallyFailed"}) == nil {
		t.Error("PartiallyFailed accepted")
	}
	keys := []string{"backups/n/n.tar.gz"}
	if BucketVerdict("b", "n", keys, 400, "InvalidRequest") != nil {
		t.Error("an SSE-C object was refused")
	}
	if BucketVerdict("b", "n", keys, 403, "AccessDenied") == nil {
		t.Error("a 403 read as encrypted — it is could-not-tell")
	}
	if BucketVerdict("b", "n", nil, 400, "") == nil {
		t.Error("a missing tarball passed")
	}
	cm := []byte(`{"metadata":{"labels":{"velero.io/restore-name":"r"}},"data":{"nonce":"N"}}`)
	sec := []byte(`{"metadata":{"labels":{"velero.io/restore-name":"r"}},"data":{"nonce":"` + base64.StdEncoding.EncodeToString([]byte("N")) + `"}}`)
	if err := CanaryVerdict(cm, sec, "N", "r"); err != nil {
		t.Error(err)
	}
	unlabelled := []byte(`{"metadata":{},"data":{"nonce":"N"}}`)
	if CanaryVerdict(unlabelled, sec, "N", "r") == nil {
		t.Error("a canary the restore did not recreate passed")
	}
	if CanaryVerdict(cm, sec, "M", "r") == nil {
		t.Error("a wrong nonce passed")
	}
}

// The canary manifest is what the fake restores; pin that it carries a Secret.
func TestCanaryManifestHoldsASecret(t *testing.T) {
	m := CanaryManifest("N")
	for _, want := range []string{`"kind":"Secret"`, `"kind":"ConfigMap"`, `"name":"` + DrillNamespace + `"`} {
		if !strings.Contains(m, want) {
			t.Errorf("canary manifest lacks %s", want)
		}
	}
}
