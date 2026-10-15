package teardown

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
)

type guardFakeS3 struct {
	mu       sync.Mutex
	versions []baosnap.Version
	deletes  int
}

func (f *guardFakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	_, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && q.Has("versions"):
		var b strings.Builder
		b.WriteString("<ListVersionsResult>")
		for _, v := range f.versions {
			tag := "Version"
			if v.DeleteMarker {
				tag = "DeleteMarker"
			}
			fmt.Fprintf(&b, "<%s><Key>%s</Key><VersionId>%s</VersionId></%s>", tag, v.Key, v.VersionID, tag)
		}
		b.WriteString("<IsTruncated>false</IsTruncated></ListVersionsResult>")
		_, _ = w.Write([]byte(b.String()))
	case r.Method == http.MethodDelete:
		f.deletes++
		for i, v := range f.versions {
			if v.Key == key && v.VersionID == q.Get("versionId") {
				f.versions = append(f.versions[:i], f.versions[i+1:]...)
				break
			}
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

type guardFakeAPI struct {
	buckets []map[string]any
	listErr error
}

func (a guardFakeAPI) ListObjectStorageBuckets(context.Context) ([]map[string]any, error) {
	return a.buckets, a.listErr
}
func (guardFakeAPI) CreateObjectStorageKeyBuckets(context.Context, string, string, []string, string) (map[string]any, error) {
	return nil, fmt.Errorf("not used: the store seam is replaced")
}
func (guardFakeAPI) DeleteObjectStorageKey(context.Context, uint64) error { return nil }

// guardHarness: an instance with deployment prod, a fake account and a fake
// versioned bucket. Returns the bucket and the permissions each minted key asked for.
func guardHarness(t *testing.T, api guardFakeAPI, vs ...baosnap.Version) (*guardFakeS3, *[]string) {
	t.Helper()
	root := t.TempDir()
	lz := "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata:\n  name: inst\nspec:\n  instance:\n    upstreamOrg: akamai-consulting\n    repo: acme/inst\n    forge: github\n"
	env := "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: ClusterDefinition\nmetadata:\n  name: prod\nspec:\n  cluster:\n    clusterLabel: inst-prod\n    region: us-ord\n    k8sVersion: v1.33.6+lke7\n    nodePool: {type: g8-dedicated-8-4, count: 5}\n    bootstrap:\n      name: inst-prod\n      managedAppPlatform: true\n    objectStorage:\n      cluster: us-ord-10\n"
	_ = os.WriteFile(filepath.Join(root, "landingzone.yaml"), []byte(lz), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "environments"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "environments", "prod.yaml"), []byte(env), 0o644)
	t.Chdir(root)
	t.Setenv("LINODE_API_TOKEN", "lin")

	f := &guardFakeS3{versions: vs}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	perms := &[]string{}
	oc, os_ := backupGuardClient, backupGuardStore
	backupGuardClient = func(string) backupGuardAPI { return api }
	backupGuardStore = func(_ context.Context, _ baosnap.KeyAPI, _, _, bucket, _, p string) (baosnap.Store, func(), error) {
		if bucket != "inst-openbao-backup-prod" {
			t.Errorf("guard opened %q, not the spec-derived backup bucket", bucket)
		}
		*perms = append(*perms, p)
		return baosnap.Store{AccessKey: "AK", SecretKey: "SK", Endpoint: srv.URL, Bucket: bucket, HTTP: srv.Client()}, func() {}, nil
	}
	t.Cleanup(func() { backupGuardClient, backupGuardStore = oc, os_ })
	return f, perms
}

var backupBucketPresent = guardFakeAPI{buckets: []map[string]any{{"label": "inst-openbao-backup-prod", "region": "us-ord"}}}

// THE SCAR: destroying object-storage used to drain the backup bucket with every
// other one. Without consent it must refuse while any version is left — noncurrent
// ones included — and delete nothing.
func TestBackupGuardRefusesWithoutConsent(t *testing.T) {
	f, perms := guardHarness(t, backupBucketPresent,
		baosnap.Version{Key: "openbao-raft/a.snap", VersionID: "1"},
		baosnap.Version{Key: "openbao-raft/b.snap", VersionID: "2"}, // pruned: only a noncurrent version left
		baosnap.Version{Key: "openbao-raft/b.snap", VersionID: "dm", DeleteMarker: true})
	err := RunOpenBaoBackupGuard("prod", false, true)
	if err == nil || !strings.Contains(err.Error(), "refusing") || !strings.Contains(err.Error(), "2 OpenBao snapshot version") {
		t.Errorf("guard without consent = %v, want a refusal counting both data versions", err)
	}
	if f.deletes != 0 {
		t.Errorf("the refusal deleted %d version(s)", f.deletes)
	}
	if len(*perms) != 1 || (*perms)[0] != "read_only" {
		t.Errorf("a refusing guard minted %v; it only needs to read", *perms)
	}
}

func TestBackupGuardDeletesEverythingWithConsentAndProvesIt(t *testing.T) {
	f, _ := guardHarness(t, backupBucketPresent,
		baosnap.Version{Key: "openbao-raft/a.snap", VersionID: "1"},
		baosnap.Version{Key: "openbao-raft/a.snap", VersionID: "0"},
		baosnap.Version{Key: "openbao-raft/b.snap", VersionID: "dm", DeleteMarker: true})
	if err := RunOpenBaoBackupGuard("prod", true, false); err == nil {
		t.Error("--allow-delete without --yes deleted backups")
	}
	if f.deletes != 0 {
		t.Fatal("deleted without --yes")
	}
	if err := RunOpenBaoBackupGuard("prod", true, true); err != nil {
		t.Fatal(err)
	}
	if len(f.versions) != 0 || f.deletes != 3 {
		t.Errorf("after a consented guard %d version(s) remain, %d deletes", len(f.versions), f.deletes)
	}
}

func TestBackupGuardPassesAnEmptyOrAbsentBucket(t *testing.T) {
	// Only a bare delete marker: nothing recoverable, but it would block the bucket
	// delete — so it goes, even without consent.
	f, _ := guardHarness(t, backupBucketPresent, baosnap.Version{Key: "x", VersionID: "dm", DeleteMarker: true})
	if err := RunOpenBaoBackupGuard("prod", false, true); err != nil {
		t.Fatal(err)
	}
	if len(f.versions) != 0 {
		t.Error("a bare delete marker was left to block the bucket delete")
	}
	// No bucket at all (state predating it, or already destroyed).
	_, _ = guardHarness(t, guardFakeAPI{buckets: []map[string]any{{"label": "inst-loki-chunks-prod", "region": "us-ord"}}},
		baosnap.Version{Key: "openbao-raft/a.snap", VersionID: "1"})
	if err := RunOpenBaoBackupGuard("prod", false, true); err != nil {
		t.Errorf("absent bucket: %v", err)
	}
}

// Fail closed: an account listing that errors is not "no bucket".
func TestBackupGuardFailsClosed(t *testing.T) {
	guardHarness(t, guardFakeAPI{listErr: fmt.Errorf("503")})
	if err := RunOpenBaoBackupGuard("prod", false, true); err == nil {
		t.Error("a failed bucket listing let the destroy proceed")
	}
}

func TestBackupGuardVerdict(t *testing.T) {
	for _, tc := range []struct {
		data, markers int
		allow, del    bool
		err           bool
	}{
		{0, 0, false, false, false},
		{0, 2, false, true, false},
		{1, 0, false, false, true},
		{1, 1, true, true, false},
	} {
		del, err := BackupGuardVerdict("b", tc.data, tc.markers, tc.allow)
		if del != tc.del || (err != nil) != tc.err {
			t.Errorf("BackupGuardVerdict(%d,%d,%v) = %v,%v", tc.data, tc.markers, tc.allow, del, err)
		}
	}
}

// The destroy job must run the guard BEFORE it drains anything, with consent
// wired from drain_data_buckets — and the drain must no longer touch the backup
// bucket itself.
func TestDestroyJobGuardsTheBackupsBeforeDraining(t *testing.T) {
	b, err := os.ReadFile("../../../../../instance-template/.github/workflows/llz-terraform.yml")
	if err != nil {
		t.Fatal(err)
	}
	wf := string(b)
	job := wf[strings.Index(wf, "  destroy-object-storage:"):]
	if i := strings.Index(job, "\n  apply-databases:"); i > 0 {
		job = job[:i]
	}
	g := strings.Index(job, "llz --yes ci openbao-backup-destroy-guard")
	d := strings.Index(job, "- name: Drain S3 buckets before destroy")
	x := strings.Index(job, "llz ci tf-destroy")
	if g < 0 || d < 0 || x < 0 || !(g < d && d < x) {
		t.Fatalf("destroy-object-storage must run the guard, then the drain, then tf-destroy (guard=%d drain=%d destroy=%d)", g, d, x)
	}
	if !strings.Contains(job[g:d], `--allow-delete="${DRAIN_DATA_BUCKETS:-false}"`) || !strings.Contains(job[:d], "DRAIN_DATA_BUCKETS: ${{ inputs.drain_data_buckets }}") {
		t.Error("the guard's consent is not wired from inputs.drain_data_buckets")
	}
	if strings.Contains(job[d:x], "openbao_backup_bucket") {
		t.Error("the drain step still drains the OpenBao backup bucket itself")
	}
}

// The Velero bucket is held to the same rule by the same verb: a destroy without
// consent refuses while it holds any version, even when the OpenBao bucket is
// empty — and refuses BEFORE emptying anything, so the bare marker in the OpenBao
// bucket is still there afterwards. With consent both are emptied.
func TestBackupGuardAlsoGuardsTheVeleroBucket(t *testing.T) {
	both := guardFakeAPI{buckets: []map[string]any{
		{"label": "inst-openbao-backup-prod", "region": "us-ord"},
		{"label": "inst-velero-prod", "region": "us-ord"},
	}}
	guardHarness(t, both) // spec, token, seams; the store is replaced below
	bao := &guardFakeS3{versions: []baosnap.Version{{Key: "x", VersionID: "dm", DeleteMarker: true}}}
	vel := &guardFakeS3{versions: []baosnap.Version{
		{Key: "backups/velero-cluster-1/velero-cluster-1.tar.gz", VersionID: "1"},
		{Key: "backups/velero-cluster-1/velero-backup.json", VersionID: "1"},
	}}
	srvs := map[string]*httptest.Server{}
	for name, f := range map[string]*guardFakeS3{"inst-openbao-backup-prod": bao, "inst-velero-prod": vel} {
		srv := httptest.NewServer(f)
		t.Cleanup(srv.Close)
		srvs[name] = srv
	}
	backupGuardStore = func(_ context.Context, _ baosnap.KeyAPI, _, _, bucket, _, _ string) (baosnap.Store, func(), error) {
		srv, ok := srvs[bucket]
		if !ok {
			t.Fatalf("guard opened unexpected bucket %q", bucket)
		}
		return baosnap.Store{AccessKey: "AK", SecretKey: "SK", Endpoint: srv.URL, Bucket: bucket, HTTP: srv.Client()}, func() {}, nil
	}

	err := RunOpenBaoBackupGuard("prod", false, true)
	if err == nil || !strings.Contains(err.Error(), "inst-velero-prod") || !strings.Contains(err.Error(), "2 Velero backup object version") {
		t.Fatalf("guard without consent = %v, want a refusal naming the velero bucket", err)
	}
	if bao.deletes != 0 || vel.deletes != 0 {
		t.Errorf("a refusal deleted versions (openbao=%d velero=%d); every bucket must be judged before any is emptied", bao.deletes, vel.deletes)
	}
	if err := RunOpenBaoBackupGuard("prod", true, true); err != nil {
		t.Fatal(err)
	}
	if len(bao.versions) != 0 || len(vel.versions) != 0 {
		t.Errorf("after a consented guard: openbao %d, velero %d version(s) left", len(bao.versions), len(vel.versions))
	}
}

// The cnpg bucket (apl-core's database backups) is the third bucket the guard
// holds to the same rule: a version there refuses the destroy without consent,
// with nothing deleted from any bucket; consent empties it.
func TestBackupGuardAlsoGuardsTheCnpgBucket(t *testing.T) {
	all := guardFakeAPI{buckets: []map[string]any{
		{"label": "inst-openbao-backup-prod", "region": "us-ord"},
		{"label": "inst-velero-prod", "region": "us-ord"},
		{"label": "inst-cnpg-prod", "region": "us-ord"},
	}}
	guardHarness(t, all)
	fakes := map[string]*guardFakeS3{
		"inst-openbao-backup-prod": {},
		"inst-velero-prod":         {},
		"inst-cnpg-prod": {versions: []baosnap.Version{
			{Key: "keycloak-1/keycloak-db/base/20261015T000700/backup.info", VersionID: "1"},
			{Key: "keycloak-1/keycloak-db/wals/0000000100000000/000000010000000000000003.gz", VersionID: "1"},
		}},
	}
	srvs := map[string]*httptest.Server{}
	for name, f := range fakes {
		srv := httptest.NewServer(f)
		t.Cleanup(srv.Close)
		srvs[name] = srv
	}
	backupGuardStore = func(_ context.Context, _ baosnap.KeyAPI, _, _, bucket, _, _ string) (baosnap.Store, func(), error) {
		srv, ok := srvs[bucket]
		if !ok {
			t.Fatalf("guard opened unexpected bucket %q", bucket)
		}
		return baosnap.Store{AccessKey: "AK", SecretKey: "SK", Endpoint: srv.URL, Bucket: bucket, HTTP: srv.Client()}, func() {}, nil
	}
	err := RunOpenBaoBackupGuard("prod", false, true)
	if err == nil || !strings.Contains(err.Error(), "inst-cnpg-prod") || !strings.Contains(err.Error(), "2 CNPG database backup object version") {
		t.Fatalf("guard without consent = %v, want a refusal naming the cnpg bucket", err)
	}
	if fakes["inst-cnpg-prod"].deletes != 0 {
		t.Error("a refusal deleted CNPG backups")
	}
	if err := RunOpenBaoBackupGuard("prod", true, true); err != nil {
		t.Fatal(err)
	}
	if n := len(fakes["inst-cnpg-prod"].versions); n != 0 {
		t.Errorf("after a consented guard the cnpg bucket still holds %d version(s)", n)
	}
}
