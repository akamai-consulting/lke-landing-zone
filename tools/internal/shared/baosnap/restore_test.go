package baosnap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeKeys struct {
	minted  []string // labels
	perms   []string
	deleted []uint64
}

func (f *fakeKeys) CreateObjectStorageKeyBuckets(_ context.Context, label, _ string, buckets []string, perms string) (map[string]any, error) {
	f.minted = append(f.minted, label+":"+strings.Join(buckets, ","))
	f.perms = append(f.perms, perms)
	return map[string]any{"id": json.Number("77"), "access_key": "AK", "secret_key": "SK"}, nil
}
func (f *fakeKeys) DeleteObjectStorageKey(_ context.Context, id uint64) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func TestTempStoreMintsScopedAndRevokes(t *testing.T) {
	f, s := newFake(t)
	f.denyN = 2 // a fresh key 403s until it propagates
	old, oldI := KeyPropagationBudget, KeyPollInterval
	KeyPropagationBudget, KeyPollInterval = 20*time.Second, 10*time.Millisecond
	t.Cleanup(func() { KeyPropagationBudget, KeyPollInterval = old, oldI })
	keys := &fakeKeys{}
	st, revoke, err := TempStore(context.Background(), keys, "llz-restore-e2e", "us-ord-10", s.Bucket, s.Endpoint, "read_only")
	if err != nil {
		t.Fatal(err)
	}
	if keys.minted[0] != "llz-restore-e2e:"+s.Bucket || keys.perms[0] != "read_only" {
		t.Errorf("minted %v with %v — want one read_only key on the backup bucket only", keys.minted, keys.perms)
	}
	if st.AccessKey != "AK" {
		t.Errorf("store = %+v", st)
	}
	revoke()
	if len(keys.deleted) != 1 || keys.deleted[0] != 77 {
		t.Errorf("revoke deleted %v, want [77]", keys.deleted)
	}
	if _, _, err := TempStore(context.Background(), keys, "x", "", s.Bucket, s.Endpoint, "read_only"); err == nil {
		t.Error("TempStore must refuse to mint against an unlocated bucket")
	}
}

func TestFetchRefusesACorruptSnapshot(t *testing.T) {
	f, s := newFake(t)
	good := goodArchive(t)
	newer := ObjectKey(time.Unix(2000, 0), "c1")
	f.objects[ObjectKey(time.Unix(1000, 0), "c1")] = good
	f.objects[newer] = []byte("truncated garbage")
	if _, _, _, err := Fetch(context.Background(), s, ""); err == nil || !strings.Contains(err.Error(), "refusing to restore") {
		t.Errorf("Fetch of a corrupt newest snapshot = %v, want a refusal", err)
	}
	snap, data, m, err := Fetch(context.Background(), s, ObjectKey(time.Unix(1000, 0), "c1"))
	if err != nil || !bytes.Equal(data, good) || m.Index != 42 || snap.ClusterID != "c1" {
		t.Errorf("Fetch named = %+v, %d bytes, %+v, %v", snap, len(data), m, err)
	}
}

// fakeBao serves health and accepts restores; health flips through `after` once a
// restore has landed, to exercise WaitServing.
type fakeBao struct {
	health   Health
	after    Health
	restored []byte
	forced   bool
	reject   string
}

func (b *fakeBao) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/sys/health":
		h := b.health
		if b.restored != nil {
			h = b.after
		}
		_ = json.NewEncoder(w).Encode(h)
	case "/v1/sys/storage/raft/snapshot", "/v1/sys/storage/raft/snapshot-force":
		if b.reject != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, b.reject)
			return
		}
		b.restored, _ = io.ReadAll(r.Body)
		b.forced = strings.HasSuffix(r.URL.Path, "-force")
		w.WriteHeader(http.StatusNoContent)
	}
}

func target(t *testing.T, b *fakeBao) Target {
	srv := httptest.NewServer(b)
	t.Cleanup(srv.Close)
	return Target{Addr: srv.URL, HTTP: srv.Client(), Close: func() {}}
}

func TestApplyGuards(t *testing.T) {
	ctx := context.Background()
	snap := Snapshot{Key: ObjectKey(time.Unix(1, 0), "c1"), ClusterID: "c1"}
	data := []byte("snapshot")
	up := Health{Initialized: true, ClusterID: "c1"}
	cases := map[string]struct {
		live  Health
		force bool
		want  string
	}{
		"uninitialized":    {Health{}, false, "not initialized"},
		"sealed":           {Health{Initialized: true, Sealed: true, ClusterID: "c1"}, false, "sealed"},
		"standby":          {Health{Initialized: true, Standby: true, ClusterID: "c1"}, false, "standby"},
		"foreign no force": {Health{Initialized: true, ClusterID: "peer"}, false, "HA pair"},
	}
	for name, c := range cases {
		b := &fakeBao{health: c.live, after: up}
		tg := target(t, b)
		p, err := Inspect(ctx, tg, snap, Manifest{})
		if err != nil {
			t.Fatal(err)
		}
		err = Apply(ctx, tg, "root", p, data, c.force, time.Second)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Apply = %v, want %q", name, err, c.want)
		}
		if b.restored != nil {
			t.Errorf("%s: a guarded restore still posted the snapshot", name)
		}
	}
	// Foreign + force: posts to snapshot-force.
	b := &fakeBao{health: Health{Initialized: true, ClusterID: "rebuilt"}, after: up}
	tg := target(t, b)
	p, _ := Inspect(ctx, tg, snap, Manifest{})
	if err := Apply(ctx, tg, "root", p, data, true, time.Second); err != nil || !b.forced {
		t.Errorf("forced restore = %v (forced=%v)", err, b.forced)
	}
	// Same cluster: a plain restore, then it waits for the node to serve.
	b = &fakeBao{health: up, after: up}
	tg = target(t, b)
	p, _ = Inspect(ctx, tg, snap, Manifest{})
	if !p.SameCluster {
		t.Fatal("Inspect did not match the cluster ids")
	}
	if err := Apply(ctx, tg, "root", p, data, false, time.Second); err != nil || b.forced || !bytes.Equal(b.restored, data) {
		t.Errorf("same-cluster restore = %v (forced=%v)", err, b.forced)
	}
}

func TestApplyFailsWhenTheNodeNeverComesBack(t *testing.T) {
	b := &fakeBao{health: Health{Initialized: true, ClusterID: "c1"}, after: Health{Initialized: true, Sealed: true, ClusterID: "c1"}}
	tg := target(t, b)
	p, _ := Inspect(context.Background(), tg, Snapshot{ClusterID: "c1"}, Manifest{})
	err := Apply(context.Background(), tg, "root", p, []byte("x"), false, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not come back unsealed") {
		t.Errorf("a restore that leaves the node sealed must fail, got %v", err)
	}
}

func TestApplyExplainsOpenBaosOwnForeignRefusal(t *testing.T) {
	// Ids match (e.g. the live id could not be read and the operator forced past
	// nothing), but OpenBao still refuses: the error must name --force and the seal key.
	b := &fakeBao{health: Health{Initialized: true, ClusterID: "c1"},
		reject: `{"errors":["failed to verify sealed hash: message authentication failed"]}`}
	tg := target(t, b)
	p, _ := Inspect(context.Background(), tg, Snapshot{ClusterID: "c1"}, Manifest{})
	err := Apply(context.Background(), tg, "root", p, []byte("x"), false, time.Second)
	if !errors.Is(err, ErrForeignSnapshot) || !strings.Contains(err.Error(), "OPENBAO_SEAL_KEY") {
		t.Errorf("Apply = %v", err)
	}
}
