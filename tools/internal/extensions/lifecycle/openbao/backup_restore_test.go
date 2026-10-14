package openbao

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
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

func restoreSnapshotBytes() []byte {
	meta, state := []byte(`{"Index":7,"Term":1}`), []byte("ciphertext")
	sum := func(n string, d []byte) string {
		s := sha256.Sum256(d)
		return hex.EncodeToString(s[:]) + "  " + n + "\n"
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

// restoreFixture: one bucket holding one snapshot from cluster c-1, and a live
// OpenBao whose cluster id the test chooses.
type restoreFixture struct {
	liveID   string
	restored int
	forced   bool
}

func (f *restoreFixture) bao(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/sys/health":
		_ = json.NewEncoder(w).Encode(map[string]any{"initialized": true, "cluster_id": f.liveID})
	case strings.HasPrefix(r.URL.Path, "/v1/sys/storage/raft/snapshot"):
		if r.Header.Get("X-Vault-Token") != "root" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.restored++
		f.forced = strings.HasSuffix(r.URL.Path, "-force")
		w.WriteHeader(http.StatusNoContent)
	}
}

type pinCluster struct{}

func (pinCluster) Run(args ...string) ([]byte, error) {
	if strings.Contains(strings.Join(args, " "), "openbao-active=true") {
		return []byte("platform-openbao-2"), nil
	}
	return []byte("ctx-under-test"), nil
}
func (pinCluster) Combined(...string) string { return "" }
func (pinCluster) Permits(...string) error   { return nil }

func withRestoreFixture(t *testing.T, liveID string) (*restoreFixture, string) {
	t.Helper()
	f := &restoreFixture{liveID: liveID}
	baosrv := httptest.NewServer(http.HandlerFunc(f.bao))
	t.Cleanup(baosrv.Close)
	key := baosnap.ObjectKey(time.Date(2026, 10, 13, 18, 23, 0, 0, time.UTC), "c-1")
	data := restoreSnapshotBytes()
	s3srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") != "" {
			fmt.Fprintf(w, "<ListBucketResult><Contents><Key>%s</Key><Size>%d</Size></Contents><IsTruncated>false</IsTruncated></ListBucketResult>", key, len(data))
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(s3srv.Close)

	ok, oc, op, os_, oset := restoreKeys, restoreCluster, restorePF, restoreStore, restoreSettle
	restoreKeys = func(string) baosnap.KeyAPI { return nil }
	restoreCluster = func() capability.Cluster { return pinCluster{} }
	restorePF = func(pod string) (string, func(), error) {
		if pod != "platform-openbao-2" {
			t.Errorf("port-forwarded to %s, not the active pod", pod)
		}
		return baosrv.URL, func() {}, nil
	}
	restoreStore = func(_ context.Context, _ baosnap.KeyAPI, label, _, bucket, _, perms string) (baosnap.Store, func(), error) {
		if perms != "read_only" || !strings.HasPrefix(label, "llz-restore-") || bucket != "inst-openbao-backup-prod" {
			t.Errorf("restore minted %s %q on %s — want a read_only llz-restore-* key on the spec-derived bucket", perms, label, bucket)
		}
		return baosnap.Store{AccessKey: "AK", SecretKey: "SK", Endpoint: s3srv.URL, Bucket: bucket, HTTP: s3srv.Client()}, func() {}, nil
	}
	restoreSettle = time.Second
	t.Cleanup(func() { restoreKeys, restoreCluster, restorePF, restoreStore, restoreSettle = ok, oc, op, os_, oset })

	root := t.TempDir()
	lz := "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata:\n  name: inst\nspec:\n  instance:\n    upstreamOrg: akamai-consulting\n    repo: acme/inst\n    forge: github\n"
	env := "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: ClusterDefinition\nmetadata:\n  name: prod\nspec:\n  cluster:\n    clusterLabel: inst-prod\n    region: us-ord\n    k8sVersion: v1.33.6+lke7\n    nodePool: {type: g8-dedicated-8-4, count: 5}\n    bootstrap:\n      name: inst-prod\n      managedAppPlatform: true\n    objectStorage:\n      cluster: us-ord-10\n"
	_ = os.WriteFile(filepath.Join(root, "landingzone.yaml"), []byte(lz), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "environments"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "environments", "prod.yaml"), []byte(env), 0o644)
	t.Chdir(root)
	t.Setenv("LINODE_API_TOKEN", "lin")
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	return f, key
}

func TestRestoreRefusesAnImplicitSnapshotChoice(t *testing.T) {
	f, _ := withRestoreFixture(t, "c-1")
	if err := RunRestore(RestoreOpts{Region: "prod", Yes: true, Confirm: "restore:prod"}); err == nil {
		t.Error("restore with neither --snapshot nor --latest must refuse")
	}
	if err := RunRestore(RestoreOpts{Region: "prod", Latest: true, Snapshot: "x", Yes: true, Confirm: "restore:prod"}); err == nil {
		t.Error("restore with both --snapshot and --latest must refuse")
	}
	if f.restored != 0 {
		t.Error("a refused restore still restored")
	}
}

func TestRestoreIsADryRunWithoutYes(t *testing.T) {
	f, _ := withRestoreFixture(t, "c-1")
	if err := RunRestore(RestoreOpts{Region: "prod", Latest: true}); err != nil {
		t.Fatal(err)
	}
	if f.restored != 0 {
		t.Error("the default (no --yes) restored")
	}
}

func TestRestoreNeedsTheExactConfirmationAndRoot(t *testing.T) {
	f, key := withRestoreFixture(t, "c-1")
	for _, c := range []string{"", "restore:staging", "prod", "restore: prod"} {
		if err := RunRestore(RestoreOpts{Region: "prod", Snapshot: key, Yes: true, Confirm: c}); err == nil {
			t.Errorf("--confirm %q accepted", c)
		}
	}
	t.Setenv("OPENBAO_ROOT_TOKEN", "")
	if err := RunRestore(RestoreOpts{Region: "prod", Snapshot: key, Yes: true, Confirm: "restore:prod"}); err == nil ||
		!strings.Contains(err.Error(), "regen-root") {
		t.Errorf("no root token: %v", err)
	}
	if f.restored != 0 {
		t.Error("a refused restore still restored")
	}
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	if err := RunRestore(RestoreOpts{Region: "prod", Snapshot: key, Yes: true, Confirm: "restore:prod"}); err != nil {
		t.Fatal(err)
	}
	if f.restored != 1 || f.forced {
		t.Errorf("restored=%d forced=%v, want one plain restore", f.restored, f.forced)
	}
}

// THE HA-PAIR GUARD. A snapshot whose cluster id is not the live cluster's is the
// other half of a pair as often as a rebuild — refuse without --force.
func TestRestoreRefusesAForeignClusterWithoutForce(t *testing.T) {
	f, key := withRestoreFixture(t, "peer-cluster")
	err := RunRestore(RestoreOpts{Region: "prod", Snapshot: key, Yes: true, Confirm: "restore:prod"})
	if err == nil || !strings.Contains(err.Error(), "HA pair") {
		t.Errorf("foreign snapshot without --force = %v", err)
	}
	if f.restored != 0 {
		t.Fatal("restored a foreign snapshot without --force")
	}
	if err := RunRestore(RestoreOpts{Region: "prod", Snapshot: key, Yes: true, Confirm: "restore:prod", Force: true}); err != nil {
		t.Fatal(err)
	}
	if !f.forced {
		t.Error("--force did not use snapshot-force")
	}
}

func TestBackupsListsNewestFirst(t *testing.T) {
	withRestoreFixture(t, "c-1")
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := RunBackups("prod")
	os.Stdout = old
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if err != nil || !strings.Contains(string(out), "openbao-raft/20261013T182300Z_c-1.snap") {
		t.Errorf("RunBackups = %v\n%s", err, out)
	}
}

// The restore binding is declared beside regen-root with the same shape: it puts
// secret material back into a running platform.
func TestRestoreBindingMatchesRegenRoot(t *testing.T) {
	r, g := Extension().MustBinding("restore"), Extension().MustBinding("regen-root")
	if r.State != g.State || r.Requires != g.Requires || len(r.Grants) != len(g.Grants) {
		t.Errorf("restore %+v drifted from regen-root %+v", r, g)
	}
}
