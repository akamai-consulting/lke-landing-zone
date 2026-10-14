package openbaobackup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

func TestExtensionValidates(t *testing.T) {
	for _, err := range Extension().Validate() {
		t.Errorf("openbao-backup does not validate: %v", err)
	}
}

// The drill's handles are assembled from its own binding at init; a binding that
// lost a grant would panic there. Assemble them again here so a unit run sees it.
func TestDrillHandlesAssembleFromTheDeclaration(t *testing.T) {
	d := defaultDeps()
	if err := d.Writer.PermitsWrite(); err != nil {
		t.Errorf("the drill's Writer refuses writes: %v — restore-drill lost cluster-write", err)
	}
	if err := capability.CloudFor(DrillBinding()).Permits(http.MethodPost); err != nil {
		t.Errorf("the drill cannot mint its temporary key: %v — restore-drill lost cloud-mutate", err)
	}
}

// The producer needs no cluster-write and no cloud grant: it talks to OpenBao and
// one bucket. Widening it would hand the CronJob's identity a reach its code never
// uses — pin the absence.
func TestTheBackupBindingStaysNarrow(t *testing.T) {
	b := Extension().MustBinding("backup")
	for _, g := range b.Grants {
		switch g {
		case "cluster-read", "secret-custody":
		default:
			t.Errorf("backup binding declares %q — the CronJob never uses it", g)
		}
	}
}

// ── a fake OpenBao whose snapshot really captures its KV state ──────────────

type fakeBao struct {
	mu        sync.Mutex
	clusterID string
	kv        map[string]map[string]string
	index     int
	// inertRestore accepts a restore and changes nothing — the failure the drill's
	// canary exists to catch.
	inertRestore bool
	restores     int
}

func newFakeBao(clusterID string, s3URL, bucket string) *fakeBao {
	return &fakeBao{clusterID: clusterID, kv: map[string]map[string]string{
		"obj/openbao-backup": {"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK", "BUCKET": bucket, "ENDPOINT": s3URL},
	}}
}

func (b *fakeBao) archive() []byte {
	state, _ := json.Marshal(b.kv)
	b.index++
	meta := []byte(fmt.Sprintf(`{"Index":%d,"Term":1}`, b.index))
	sum := func(name string, d []byte) string {
		s := sha256.Sum256(d)
		return hex.EncodeToString(s[:]) + "  " + name + "\n"
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, m := range []struct {
		n string
		d []byte
	}{{"meta.json", meta}, {"state.bin", state}, {"SHA256SUMS", []byte(sum("meta.json", meta) + sum("state.bin", state))}} {
		_ = tw.WriteHeader(&tar.Header{Name: m.n, Mode: 0o600, Size: int64(len(m.d))})
		_, _ = tw.Write(m.d)
	}
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

func (b *fakeBao) restore(data []byte) error {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err != nil {
			return err
		}
		if h.Name == "state.bin" {
			body, _ := io.ReadAll(tr)
			kv := map[string]map[string]string{}
			if err := json.Unmarshal(body, &kv); err != nil {
				return err
			}
			b.kv = kv
			return nil
		}
	}
}

func (b *fakeBao) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := r.URL.Path
	switch {
	case p == "/v1/auth/kubernetes/login":
		var req struct{ Role, JWT string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Role != baosnap.Role {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"auth":{"client_token":"backup-token"}}`)
	case p == "/v1/sys/health":
		_ = json.NewEncoder(w).Encode(map[string]any{"initialized": true, "sealed": false, "standby": false, "cluster_id": b.clusterID})
	case p == "/v1/sys/storage/raft/snapshot" && r.Method == http.MethodGet:
		if r.Header.Get("X-Vault-Token") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write(b.archive())
	case strings.HasPrefix(p, "/v1/sys/storage/raft/snapshot") && r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		b.restores++
		if !b.inertRestore {
			if err := b.restore(body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, err.Error())
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(p, "/v1/secret/data/"):
		key := strings.TrimPrefix(p, "/v1/secret/data/")
		switch r.Method {
		case http.MethodGet:
			d, ok := b.kv[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": d, "metadata": map[string]any{"version": 1}}})
		default:
			var body struct {
				Data map[string]string `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			b.kv[key] = body.Data
			_, _ = io.WriteString(w, `{}`)
		}
	case strings.HasPrefix(p, "/v1/secret/metadata/") && r.Method == http.MethodDelete:
		delete(b.kv, strings.TrimPrefix(p, "/v1/secret/metadata/"))
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// ── a fake bucket ────────────────────────────────────────────────────────────

type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.objects[key] = b
		f.puts++
	case r.Method == http.MethodGet && key == "":
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var sb strings.Builder
		sb.WriteString("<ListBucketResult>")
		for _, k := range keys {
			fmt.Fprintf(&sb, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-10-14T00:00:00Z</LastModified></Contents>", k, len(f.objects[k]))
		}
		sb.WriteString("<IsTruncated>false</IsTruncated></ListBucketResult>")
		_, _ = io.WriteString(w, sb.String())
	case r.Method == http.MethodGet:
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(b)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

const bucket = "inst-openbao-backup-prod"

// harness wires the producer's seams at a fake OpenBao and a fake bucket.
func harness(t *testing.T) (*fakeBao, *fakeS3, *httptest.Server, *httptest.Server) {
	t.Helper()
	s3 := &fakeS3{objects: map[string][]byte{}}
	s3srv := httptest.NewServer(s3)
	t.Cleanup(s3srv.Close)
	bao := newFakeBao("c-123", s3srv.URL, bucket)
	baosrv := httptest.NewServer(bao)
	t.Cleanup(baosrv.Close)

	t.Setenv("OPENBAO_ADDR", baosrv.URL)
	oc, os_, oh, ov, on := httpClientFn, saTokenFn, storeHTTP, virtualHosted, nowFn
	httpClientFn = func(string) (*http.Client, error) { return baosrv.Client(), nil }
	saTokenFn = func() (string, error) { return "sa-jwt", nil }
	storeHTTP = s3srv.Client()
	virtualHosted = false
	tick := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	nowFn = func() time.Time { tick = tick.Add(time.Hour); return tick }
	t.Cleanup(func() { httpClientFn, saTokenFn, storeHTTP, virtualHosted, nowFn = oc, os_, oh, ov, on })
	return bao, s3, baosrv, s3srv
}

func TestRunBackupUploadsReadsBackAndPrunes(t *testing.T) {
	_, s3, _, _ := harness(t)
	ctx := context.Background()
	var last Record
	for i := 0; i < 5; i++ {
		rec, err := RunBackup(ctx, 3)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		last = rec
	}
	if last.Action != "uploaded" || last.Bucket != bucket || last.ClusterID != "c-123" || last.SHA256 == "" {
		t.Errorf("record = %+v", last)
	}
	if _, ok := s3.objects[last.Key]; !ok {
		t.Fatalf("the last run's key %s is not in the bucket", last.Key)
	}
	if n := len(s3.objects); n != 3 {
		t.Errorf("bucket holds %d snapshots after 5 runs with keep=3, want 3", n)
	}
	if last.Retained != 3 || len(last.Pruned) != 1 {
		t.Errorf("last run retained %d pruned %v", last.Retained, last.Pruned)
	}
}

func TestRunBackupFailsClosed(t *testing.T) {
	ctx := context.Background()

	t.Run("no backup key in OpenBao", func(t *testing.T) {
		bao, s3, _, _ := harness(t)
		delete(bao.kv, "obj/openbao-backup")
		_, err := RunBackup(ctx, 3)
		if err == nil || !strings.Contains(err.Error(), "mint-bootstrap-objkeys") {
			t.Errorf("err = %v, want the remedy named", err)
		}
		if s3.puts != 0 {
			t.Error("uploaded with no key")
		}
	})
	t.Run("incomplete backup key", func(t *testing.T) {
		bao, _, _, _ := harness(t)
		delete(bao.kv["obj/openbao-backup"], "BUCKET")
		if _, err := RunBackup(ctx, 3); err == nil || !strings.Contains(err.Error(), "BUCKET") {
			t.Errorf("err = %v, want the missing field named", err)
		}
	})
	t.Run("wrong role is refused at login", func(t *testing.T) {
		harness(t)
		t.Setenv("OPENBAO_KUBERNETES_ROLE", "reconciler")
		if _, err := RunBackup(ctx, 3); err == nil || !strings.Contains(err.Error(), "bao-configure") {
			t.Errorf("err = %v, want a login failure pointing at bao-configure", err)
		}
	})
	t.Run("a bucket that returns other bytes", func(t *testing.T) {
		_, _, _, s3srv := harness(t)
		// Accept the PUT, return garbage on GET: the read-back must catch it.
		s3srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, "not what was written")
			}
		})
		rec, err := RunBackup(ctx, 3)
		if err == nil || !strings.Contains(err.Error(), "read-back") {
			t.Errorf("err = %v, want a read-back mismatch", err)
		}
		if rec.Action == "uploaded" {
			t.Error("a run whose read-back failed reported action=uploaded")
		}
	})
}

// THE SPLIT CONTRACT between producer and gate: the drill learns which object to
// restore from the line the producer prints. Feed the producer's REAL printer into
// the drill's REAL parser — never a hand-written line.
func TestTheDrillParsesWhatTheProducerPrints(t *testing.T) {
	rec := Record{Event: baosnap.AuditEvent, Action: "uploaded", Bucket: bucket,
		Key: baosnap.ObjectKey(time.Unix(1, 0), "c-1"), Bytes: 10, SHA256: "ab", ClusterID: "c-1", Pruned: []string{}}
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := printRecord(rec)
	os.Stdout = old
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	logs := "some kubelet noise\n{\"event\":\"other\",\"key\":\"x\"}\n" + string(out)
	got, ok := ParseRecord(logs)
	if !ok || got.Key != rec.Key || got.Action != "uploaded" || got.SHA256 != "ab" || got.Bucket != bucket {
		t.Errorf("ParseRecord(producer output) = %+v, %v", got, ok)
	}
}

// ── the manifests, against the code that depends on them ─────────────────────

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "platform-apl")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("repo root not found")
	return ""
}

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Every name the login, the NetworkPolicy and the chart's ingress allow depend on,
// held to the one constant each side reads. A rename on either side is a backup
// that 403s or times out on every tick, and only this test would see it before
// the first scheduled run.
func TestOpenBaoBackupManifestMatchesTheRole(t *testing.T) {
	dir := "platform-apl/components/openbaoBackup/openbao-backup/"
	cron := read(t, dir+"cronjob.yaml")
	for _, want := range []string{
		"namespace: " + baosnap.Namespace,
		"name: " + baosnap.CronJob,
		"serviceAccountName: " + baosnap.ServiceAccount,
		`args: ["ci", "` + BackupCmd().Use + `"]`,
		"app.kubernetes.io/name: openbao-backup",
		"value: \"" + baosnap.Role + "\"",
		"value: \"" + DefaultActiveAddr + "\"",
		"value: \"" + DefaultTLSServerName + "\"",
		"secretName: openbao-backup-client-tls",
		"secretName: openbao-ca-bundle",
	} {
		if !strings.Contains(cron, want) {
			t.Errorf("cronjob.yaml does not contain %q", want)
		}
	}
	if sa := read(t, dir+"serviceaccount.yaml"); !strings.Contains(sa, "name: "+baosnap.ServiceAccount) ||
		!strings.Contains(sa, "namespace: "+baosnap.Namespace) {
		t.Error("serviceaccount.yaml does not declare the identity the openbao-backup role binds")
	}
	if np := read(t, dir+"network-policy.yaml"); !strings.Contains(np, "kubernetes.io/metadata.name: llz-openbao") ||
		!strings.Contains(np, "port: 8200") || !strings.Contains(np, "app.kubernetes.io/name: openbao-backup") {
		t.Error("network-policy.yaml does not allow this pod's egress to OpenBao :8200")
	}
	// The OTHER half of the hop: OpenBao's default-deny admits this pod only via
	// the chart's allowedClientPods.
	values := read(t, "kubernetes-charts/llz-openbao-platform/values.yaml")
	if !strings.Contains(values, "- namespace: "+baosnap.Namespace+"\n        name: openbao-backup") {
		t.Error("llz-openbao-platform values.yaml has no allowedClientPods entry for the backup pod — its login would time out at L4")
	}
	// The serving cert must carry the name the producer verifies against.
	cert := read(t, "kubernetes-charts/llz-openbao-platform/templates/openbao-tls-cert.yaml")
	if !strings.Contains(cert, "{{ .Values.platform.releaseName }}.{{ .Release.Namespace }}.svc.cluster.local") ||
		!strings.HasPrefix(DefaultTLSServerName, "platform-openbao.llz-openbao.svc") {
		t.Error("OPENBAO_TLS_SERVER_NAME is not a name openbao-tls carries")
	}
}

// ── the drill, end to end against the fakes ──────────────────────────────────

type fakeCluster struct {
	cronJSON string
	logs     *string
}

func (c fakeCluster) Run(args ...string) ([]byte, error) {
	j := strings.Join(args, " ")
	switch {
	case strings.Contains(j, "openbao-active=true"):
		return []byte("platform-openbao-1"), nil
	case strings.Contains(j, "get cronjob"):
		return []byte(c.cronJSON), nil
	case strings.Contains(j, "get job "+DrillJob):
		return []byte("1/"), nil
	}
	return nil, nil
}
func (c fakeCluster) Combined(args ...string) string {
	j := strings.Join(args, " ")
	switch {
	case strings.Contains(j, "get pods -l job-name="):
		return "drill-pod\n"
	case strings.Contains(j, "logs drill-pod"):
		return *c.logs
	}
	return ""
}
func (c fakeCluster) Permits(...string) error { return nil }

// fakeWriter "runs" the Job: on apply it executes the REAL producer in-process
// against the same fakes, and captures its audit line as the Job's logs.
type fakeWriter struct {
	capability.Writer
	logs    *string
	applied int
}

func (w *fakeWriter) Delete(string, string, ...string) ([]byte, error) { return nil, nil }
func (w *fakeWriter) ApplyStdin(manifest, _ string) ([]byte, error) {
	w.applied++
	var job struct {
		Metadata struct {
			OwnerReferences []map[string]any `json:"ownerReferences"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(manifest), &job); err != nil || len(job.Metadata.OwnerReferences) != 1 {
		return nil, fmt.Errorf("drill Job is not owned by the CronJob: %v", err)
	}
	rec, err := RunBackup(context.Background(), DefaultKeep)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(rec)
	*w.logs = "starting\n" + string(b) + "\n"
	return nil, nil
}

func drillInstance(t *testing.T, enabled bool) {
	t.Helper()
	root := t.TempDir()
	lz := `apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata:
  name: inst
spec:
  instance:
    upstreamOrg: akamai-consulting
    repo: acme/inst
    forge: github
`
	env := `apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata:
  name: prod
spec:
  cluster:
    clusterLabel: inst-prod
    region: us-ord
    k8sVersion: v1.33.6+lke7
    nodePool: {type: g8-dedicated-8-4, count: 5}
    bootstrap:
      name: inst-prod
      managedAppPlatform: true
    objectStorage:
      cluster: us-ord-10
  components:
    openbaoBackup: {enabled: ` + strconv.FormatBool(enabled) + `}
`
	if err := os.WriteFile(filepath.Join(root, "landingzone.yaml"), []byte(lz), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "environments"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "environments", "prod.yaml"), []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
}

func drillHarness(t *testing.T) (*fakeBao, *fakeWriter) {
	bao, _, baosrv, s3srv := harness(t)
	logs := new(string)
	w := &fakeWriter{Writer: capability.Denied(), logs: logs}
	old, oldTS := caps, tempStoreFn
	caps = Deps{
		Cluster: fakeCluster{cronJSON: `{"metadata":{"name":"openbao-backup","uid":"u-1"},` +
			`"spec":{"jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"backup"}]}}}}}}`, logs: logs},
		Writer:         w,
		Keys:           func(string) baosnap.KeyAPI { return nil },
		PortForwardPod: func(string) (string, func(), error) { return baosrv.URL, func() {}, nil },
	}
	// The consumer side reads the bucket the producer wrote, through the restore
	// path's own Fetch — only the key mint is stubbed.
	tempStoreFn = func(_ context.Context, _ baosnap.KeyAPI, _, _, b, _, perms string) (baosnap.Store, func(), error) {
		if perms != "read_only" {
			t.Errorf("the drill minted a %s key; the consumer side only reads", perms)
		}
		return baosnap.Store{AccessKey: "AK", SecretKey: "SK", Endpoint: s3srv.URL, Bucket: b, HTTP: s3srv.Client()}, func() {}, nil
	}
	ow, oc, oj, op := cronJobWait, canaryReadWait, jobWait, pollInterval
	cronJobWait, canaryReadWait, jobWait, pollInterval = time.Second, time.Second, time.Second, 10*time.Millisecond
	t.Cleanup(func() {
		caps, tempStoreFn = old, oldTS
		cronJobWait, canaryReadWait, jobWait, pollInterval = ow, oc, oj, op
	})
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	t.Setenv("LINODE_API_TOKEN", "lin")
	return bao, w
}

func TestDrillRoundTripsTheCanaryThroughTheRealProducer(t *testing.T) {
	drillInstance(t, true)
	bao, w := drillHarness(t)
	if err := RunDrill(context.Background(), "prod"); err != nil {
		t.Fatalf("drill failed against a working backup/restore: %v", err)
	}
	if w.applied != 1 || bao.restores != 1 {
		t.Errorf("applied %d Job(s), %d restore(s); want 1 and 1", w.applied, bao.restores)
	}
	if _, left := bao.kv["infra/openbao-restore-drill"]; left {
		t.Error("the drill left its canary behind")
	}
}

func TestDrillFailsWhenARestoreChangesNothing(t *testing.T) {
	drillInstance(t, true)
	bao, _ := drillHarness(t)
	bao.inertRestore = true
	err := RunDrill(context.Background(), "prod")
	if err == nil || !strings.Contains(err.Error(), "did not roll its state back") {
		t.Errorf("an inert restore must fail the drill, got %v", err)
	}
}

func TestDrillFailsWhenTheProducerWritesElsewhere(t *testing.T) {
	drillInstance(t, true)
	bao, _ := drillHarness(t)
	bao.kv["obj/openbao-backup"]["BUCKET"] = "someone-elses-bucket"
	err := RunDrill(context.Background(), "prod")
	if err == nil || !strings.Contains(err.Error(), "would not find") {
		t.Errorf("a producer writing to a bucket the restore path does not derive must fail, got %v", err)
	}
}

func TestDrillSkipsOnlyWhenTheComponentIsOff(t *testing.T) {
	drillInstance(t, false)
	_, w := drillHarness(t)
	if err := RunDrill(context.Background(), "prod"); err != nil {
		t.Fatalf("disabled component: %v", err)
	}
	if w.applied != 0 {
		t.Error("the drill ran a Job for a disabled component")
	}

	drillInstance(t, true)
	t.Setenv("OPENBAO_ROOT_TOKEN", "")
	if err := RunDrill(context.Background(), "prod"); err == nil || !strings.Contains(err.Error(), "root") {
		t.Errorf("an enabled component with no root token must fail, not skip: %v", err)
	}
}

func TestDrillJobJSONIsOwnedAndFaithful(t *testing.T) {
	cron := `{"metadata":{"name":"openbao-backup","uid":"u-1"},"spec":{"jobTemplate":{"spec":{"backoffLimit":1,"template":{"spec":{"serviceAccountName":"openbao-backup"}}}}}}`
	out, err := DrillJobJSON([]byte(cron))
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	_ = json.Unmarshal(out, &job)
	meta := job["metadata"].(map[string]any)
	if meta["name"] != DrillJob || meta["namespace"] != baosnap.Namespace {
		t.Errorf("job metadata = %v", meta)
	}
	owner := meta["ownerReferences"].([]any)[0].(map[string]any)
	if owner["kind"] != "CronJob" || owner["uid"] != "u-1" || owner["controller"] != true {
		t.Errorf("owner = %v — a drill Job not owned by the CronJob cannot supersede a failed tick", owner)
	}
	if !strings.Contains(string(out), `"serviceAccountName":"openbao-backup"`) {
		t.Error("the Job does not carry the CronJob's pod spec")
	}
	if _, err := DrillJobJSON([]byte(`{"metadata":{"name":"x"},"spec":{"jobTemplate":{"spec":{}}}}`)); err == nil {
		t.Error("a CronJob with no uid (not read from a cluster) must be refused")
	}
}
