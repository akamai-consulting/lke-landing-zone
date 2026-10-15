package baosnap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── naming ───────────────────────────────────────────────────────────────────

func TestObjectKeyRoundTrips(t *testing.T) {
	at := time.Date(2026, 10, 14, 3, 17, 5, 0, time.UTC)
	k := ObjectKey(at, "8f1c2a3b-0000-4d5e-9f00-112233445566")
	if k != "openbao-raft/20261014T031705Z_8f1c2a3b-0000-4d5e-9f00-112233445566.snap" {
		t.Fatalf("ObjectKey = %q", k)
	}
	s, ok := ParseObjectKey(k)
	if !ok || !s.Taken.Equal(at) || s.ClusterID != "8f1c2a3b-0000-4d5e-9f00-112233445566" {
		t.Fatalf("ParseObjectKey(%q) = %+v, %v", k, s, ok)
	}
	// A non-UUID or empty cluster id must still produce a key the lister parses —
	// a key the producer writes but the restore cannot see is the split contract.
	for _, id := range []string{"", "  ", "Weird ID/With*Junk"} {
		k := ObjectKey(at, id)
		if _, ok := ParseObjectKey(k); !ok {
			t.Errorf("ObjectKey(%q) = %q does not parse back", id, k)
		}
	}
}

func TestParseObjectKeyRejectsForeignObjects(t *testing.T) {
	for _, k := range []string{
		"openbao-raft/manual.snap",
		"elsewhere/20261014T031705Z_abc.snap",
		"openbao-raft/20261014T031705Z_abc.snap.tmp",
		"openbao-raft/20261014T031705Z_ABC.snap",
		"openbao-raft/2026-10-14_abc.snap",
	} {
		if _, ok := ParseObjectKey(k); ok {
			t.Errorf("ParseObjectKey(%q) accepted a key ObjectKey never produces", k)
		}
	}
}

func snapsAt(hours ...int) []Snapshot {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var objs []Object
	for _, h := range hours {
		objs = append(objs, Object{Key: ObjectKey(base.Add(time.Duration(h)*time.Hour), "c1"), Size: 10})
	}
	s, _ := Catalog(objs)
	return s
}

func TestCatalogIsNewestFirstAndSeparatesForeign(t *testing.T) {
	objs := []Object{
		{Key: ObjectKey(time.Unix(100, 0), "a")},
		{Key: "openbao-raft/hand-taken.snap"},
		{Key: ObjectKey(time.Unix(300, 0), "a")},
		{Key: ObjectKey(time.Unix(200, 0), "a")},
	}
	snaps, foreign := Catalog(objs)
	if len(snaps) != 3 || snaps[0].Taken.Unix() != 300 || snaps[2].Taken.Unix() != 100 {
		t.Errorf("Catalog order = %+v", snaps)
	}
	if len(foreign) != 1 || foreign[0] != "openbao-raft/hand-taken.snap" {
		t.Errorf("foreign = %v", foreign)
	}
}

func TestPruneKeepsNewestAndNeverTheJustWritten(t *testing.T) {
	snaps := snapsAt(0, 6, 12, 18, 24) // newest = 24
	newest := snaps[0].Key
	got := Prune(snaps, 3, newest)
	if len(got) != 2 || got[0] != snaps[3].Key || got[1] != snaps[4].Key {
		t.Errorf("Prune keep=3 = %v, want the two oldest", got)
	}
	// keep < 1 is floored: the run must never delete what it just wrote.
	for _, k := range []int{0, -5} {
		got := Prune(snaps, k, newest)
		for _, d := range got {
			if d == newest {
				t.Fatalf("Prune keep=%d deleted the snapshot just written", k)
			}
		}
		if len(got) != 4 {
			t.Errorf("Prune keep=%d removed %d, want 4 (floored to 1)", k, len(got))
		}
	}
	// A skewed clock writes a key that sorts OLDEST; it is still kept, and counts
	// toward keep.
	oldest := snaps[4].Key
	got = Prune(snaps, 2, oldest)
	for _, d := range got {
		if d == oldest {
			t.Fatal("Prune deleted the just-written snapshot because its timestamp sorted old")
		}
	}
	if len(got) != 3 {
		t.Errorf("Prune with skewed just-written kept wrong count: deleted %v", got)
	}
}

func TestPruneRefusesWhenTheListingMissedOurWrite(t *testing.T) {
	snaps := snapsAt(0, 6, 12, 18, 24)
	if got := Prune(snaps, 1, ObjectKey(time.Now(), "c1")); got != nil {
		t.Errorf("Prune acted on a listing that does not contain the just-written key: %v", got)
	}
}

func TestSelectNeverSubstitutes(t *testing.T) {
	snaps := snapsAt(0, 6)
	if s, err := Select(snaps, "", ""); err != nil || s.Key != snaps[0].Key {
		t.Errorf("Select latest = %+v, %v", s, err)
	}
	if _, err := Select(snaps, "openbao-raft/typo.snap", ""); err == nil {
		t.Error("Select returned something for a key that is not in the bucket")
	}
	if _, err := Select(nil, "", ""); err == nil {
		t.Error("Select on an empty bucket must fail, not return a zero snapshot")
	}
	if _, err := Select(snaps, snaps[1].Key, "someone-else"); err == nil {
		t.Error("Select returned a named key whose cluster disagrees with --from-cluster")
	}
}

// rebuiltBucket is the bucket after a lost-and-rebuilt OpenBao: `old` wrote the
// real history every six hours for oldN ticks; then, after the loss, the rebuilt
// cluster `new` (an empty store) kept ticking into the same bucket for newN ticks.
func rebuiltBucket(oldN, newN int) (snaps []Snapshot, oldKeys, newKeys []string) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var objs []Object
	for i := 0; i < oldN; i++ {
		k := ObjectKey(base.Add(time.Duration(i*6)*time.Hour), "old-cluster")
		oldKeys = append(oldKeys, k)
		objs = append(objs, Object{Key: k})
	}
	after := base.Add(time.Duration(oldN*6+12) * time.Hour)
	for i := 0; i < newN; i++ {
		k := ObjectKey(after.Add(time.Duration(i*6)*time.Hour), "new-cluster")
		newKeys = append(newKeys, k)
		objs = append(objs, Object{Key: k})
	}
	snaps, _ = Catalog(objs)
	return snaps, oldKeys, newKeys
}

// THE REBUILD SCAR. A rebuilt OpenBao is a new cluster id backing up an empty
// store into the same bucket. Under bucket-wide retention its empty snapshots
// pushed every pre-loss one out within a week. Simulate the whole week, tick by
// tick, through the producer's real Prune, and require the old cluster's newest
// keep to survive all of it.
func TestPruneNeverLetsARebuiltClusterBuryTheLostOnesHistory(t *testing.T) {
	const keep = 28
	snaps, oldKeys, _ := rebuiltBucket(40, 0)
	// Steady state before the loss: the old cluster pruned itself to keep.
	live := map[string]bool{}
	for _, s := range snaps {
		live[s.Key] = true
	}
	for _, k := range Prune(snaps, keep, oldKeys[len(oldKeys)-1]) {
		delete(live, k)
	}
	survivorsBefore := len(live)
	if survivorsBefore != keep {
		t.Fatalf("pre-loss retention kept %d, want %d", survivorsBefore, keep)
	}
	// Two weeks of ticks from the rebuilt cluster.
	after := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 56; i++ {
		k := ObjectKey(after.Add(time.Duration(i*6)*time.Hour), "new-cluster")
		live[k] = true
		var objs []Object
		for key := range live {
			objs = append(objs, Object{Key: key})
		}
		cat, _ := Catalog(objs)
		for _, d := range Prune(cat, keep, k) {
			delete(live, d)
		}
	}
	oldLeft, newLeft := 0, 0
	for k := range live {
		s, _ := ParseObjectKey(k)
		if s.ClusterID == "old-cluster" {
			oldLeft++
		} else {
			newLeft++
		}
	}
	if oldLeft != keep {
		t.Errorf("after two weeks of rebuilt-cluster ticks the lost cluster has %d snapshots left, want all %d — "+
			"retention is letting an empty store's backups push out the real ones", oldLeft, keep)
	}
	if newLeft != keep {
		t.Errorf("the rebuilt cluster retained %d of its own, want %d (it must still prune itself)", newLeft, keep)
	}
	// And the newest of the old cluster is the one that must never go.
	if !live[oldKeys[len(oldKeys)-1]] {
		t.Error("the lost cluster's newest snapshot was pruned")
	}
}

func TestPruneKeepsEachClustersOwnNewest(t *testing.T) {
	snaps, oldKeys, newKeys := rebuiltBucket(5, 5)
	got := Prune(snaps, 2, newKeys[len(newKeys)-1])
	want := map[string]bool{oldKeys[0]: true, oldKeys[1]: true, oldKeys[2]: true, newKeys[0]: true, newKeys[1]: true, newKeys[2]: true}
	if len(got) != len(want) {
		t.Fatalf("Prune keep=2 across two clusters deleted %v, want the three oldest of EACH", got)
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("Prune deleted %s, which is among its cluster's newest 2", k)
		}
	}
}

// After a rebuild the newest object is the rebuilt cluster's empty store, and its
// id matches the live cluster's, so `--latest` would pick it and the dry run would
// say "match". It must refuse instead, and say what IS there.
func TestSelectLatestRefusesAcrossClusters(t *testing.T) {
	snaps, oldKeys, newKeys := rebuiltBucket(3, 2)
	_, err := Select(snaps, "", "")
	if err == nil {
		t.Fatal("Select --latest on a bucket holding two clusters' snapshots picked one; it must refuse")
	}
	for _, want := range []string{"old-cluster", "new-cluster", "--from-cluster"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
	s, err := Select(snaps, "", "old-cluster")
	if err != nil || s.Key != oldKeys[len(oldKeys)-1] {
		t.Errorf("Select --latest --from-cluster old-cluster = %+v, %v; want %s", s, err, oldKeys[len(oldKeys)-1])
	}
	s, err = Select(snaps, "", "NEW-cluster ")
	if err != nil || s.Key != newKeys[len(newKeys)-1] {
		t.Errorf("Select --from-cluster is not normalised like the key: %+v, %v", s, err)
	}
	if _, err := Select(snaps, "", "nobody"); err == nil || !strings.Contains(err.Error(), "old-cluster") {
		t.Errorf("Select --from-cluster for an absent id = %v, want an error naming what is there", err)
	}
	// A named key needs no disambiguation.
	if s, err := Select(snaps, oldKeys[0], ""); err != nil || s.Key != oldKeys[0] {
		t.Errorf("Select named key in a mixed bucket = %+v, %v", s, err)
	}
}

// ── archive ──────────────────────────────────────────────────────────────────

type member struct {
	name string
	body []byte
}

func sumLine(name string, b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]) + "  " + name + "\n"
}

func buildArchive(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, m := range members {
		if err := tw.WriteHeader(&tar.Header{Name: m.name, Mode: 0o600, Size: int64(len(m.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(m.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// goodArchive mirrors what the raft snapshot writer emits: meta.json, state.bin,
// SHA256SUMS over both, and OpenBao's SHA256SUMS.sealed.
func goodArchive(t *testing.T) []byte {
	meta := []byte(`{"Version":1,"ID":"2-42-1700000000000","Index":42,"Term":2,"Size":11}`)
	state := []byte("ciphertext!")
	return buildArchive(t,
		member{"meta.json", meta},
		member{"state.bin", state},
		member{"SHA256SUMS", []byte(sumLine("meta.json", meta) + sumLine("state.bin", state))},
		member{"SHA256SUMS.sealed", []byte("opaque")},
	)
}

func TestVerifyAcceptsAnIntactSnapshot(t *testing.T) {
	m, err := Verify(goodArchive(t))
	if err != nil {
		t.Fatal(err)
	}
	if m.Index != 42 || m.Term != 2 || !m.Sealed || m.StateBytes != 11 {
		t.Errorf("manifest = %+v", m)
	}
}

func TestVerifyFailsClosed(t *testing.T) {
	meta := []byte(`{"Index":1,"Term":1}`)
	state := []byte("data")
	sums := sumLine("meta.json", meta) + sumLine("state.bin", state)
	cases := map[string][]byte{
		"empty":    nil,
		"not gzip": []byte("plain text"),
		"no SHA256SUMS — nothing was checked": buildArchive(t,
			member{"meta.json", meta}, member{"state.bin", state}),
		"empty SHA256SUMS": buildArchive(t,
			member{"meta.json", meta}, member{"state.bin", state}, member{"SHA256SUMS", nil}),
		"sums omit state.bin": buildArchive(t,
			member{"meta.json", meta}, member{"state.bin", state},
			member{"SHA256SUMS", []byte(sumLine("meta.json", meta))}),
		"corrupt state.bin": buildArchive(t,
			member{"meta.json", meta}, member{"state.bin", []byte("dat4")}, member{"SHA256SUMS", []byte(sums)}),
		"sums name a missing member": buildArchive(t,
			member{"meta.json", meta}, member{"state.bin", state},
			member{"SHA256SUMS", []byte(sums + sumLine("extra.bin", []byte("x")))}),
		"malformed sums line": buildArchive(t,
			member{"meta.json", meta}, member{"state.bin", state}, member{"SHA256SUMS", []byte("nonsense\n")}),
		"no state.bin": buildArchive(t,
			member{"meta.json", meta}, member{"SHA256SUMS", []byte(sumLine("meta.json", meta))}),
		"empty state.bin": buildArchive(t,
			member{"meta.json", meta}, member{"state.bin", nil},
			member{"SHA256SUMS", []byte(sumLine("meta.json", meta) + sumLine("state.bin", nil))}),
	}
	for name, data := range cases {
		if _, err := Verify(data); err == nil {
			t.Errorf("%s: Verify passed", name)
		}
	}
}

// ── store ────────────────────────────────────────────────────────────────────

// fakeS3 is a path-style, ListObjectsV2-speaking in-memory bucket that also
// enforces the one signing property this package owns: the query it receives is
// the query it was signed over.
type fakeS3 struct {
	mu       sync.Mutex
	bucket   string
	objects  map[string][]byte
	pageSize int
	denyN    int // answer 403 to the first denyN requests (key propagation)
	requests int
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") ||
		r.Header.Get("x-amz-content-sha256") == "" || r.Header.Get("x-amz-date") == "" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if f.denyN > 0 {
		f.denyN--
		w.Header().Set("x-amz-error-code", "AccessDenied")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	b, key, _ := strings.Cut(path, "/")
	if b != f.bucket {
		w.Header().Set("x-amz-error-code", "NoSuchBucket")
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.objects[key] = body
	case r.Method == http.MethodGet && key == "":
		f.list(w, r)
	case r.Method == http.MethodGet:
		body, ok := f.objects[key]
		if !ok {
			w.Header().Set("x-amz-error-code", "NoSuchKey")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, q.Get("prefix")) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	start := 0
	if tok := q.Get("continuation-token"); tok != "" {
		start, _ = strconv.Atoi(tok)
	}
	ps := f.pageSize
	if ps == 0 {
		ps = 1000
	}
	end := start + ps
	if end > len(keys) {
		end = len(keys)
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>`)
	for _, k := range keys[start:end] {
		fmt.Fprintf(&b, "<Contents><Key>%s</Key><LastModified>2026-10-14T03:17:05.000Z</LastModified><Size>%d</Size></Contents>", k, len(f.objects[k]))
	}
	if end < len(keys) {
		fmt.Fprintf(&b, "<IsTruncated>true</IsTruncated><NextContinuationToken>%d</NextContinuationToken>", end)
	} else {
		b.WriteString("<IsTruncated>false</IsTruncated>")
	}
	b.WriteString("</ListBucketResult>")
	_, _ = io.WriteString(w, b.String())
}

func newFake(t *testing.T) (*fakeS3, Store) {
	f := &fakeS3{bucket: "acme-openbao-backup-e2e", objects: map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, Store{AccessKey: "AK", SecretKey: "SK", Endpoint: srv.URL, Bucket: f.bucket, HTTP: srv.Client()}
}

func TestStoreRoundTripAndPaginatedList(t *testing.T) {
	f, s := newFake(t)
	f.pageSize = 2 // force pagination: the probe helpers this replaces truncated here
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.Put(ctx, ObjectKey(time.Unix(int64(i*3600), 0), "c1"), []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	f.objects["unrelated/key"] = []byte("x")
	objs, err := s.List(ctx, Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 5 {
		t.Fatalf("List returned %d objects across pages, want 5", len(objs))
	}
	data, err := s.Get(ctx, objs[0].Key)
	if err != nil || len(data) != 1 {
		t.Fatalf("Get = %v, %v", data, err)
	}
	sum, n, err := s.HashObject(ctx, objs[0].Key)
	if err != nil || n != 1 || sum != SHA256(data) {
		t.Errorf("HashObject = %s, %d, %v", sum, n, err)
	}
	if err := s.Delete(ctx, objs[0].Key); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, objs[0].Key); err != nil {
		t.Errorf("deleting an absent object must succeed: %v", err)
	}
}

// The in-cluster producer addresses the bucket virtual-hosted so the objProxy
// CoreDNS rewrite (exact-match on the endpoint host) never captures it. Pin the
// wire shape: the bucket moves into the Host, out of the path.
func TestVirtualHostedAddressing(t *testing.T) {
	var gotHost, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath = r.Host, r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	s := Store{AccessKey: "AK", SecretKey: "SK", Endpoint: srv.URL, Bucket: "acme-openbao-backup-e2e", VirtualHosted: true,
		HTTP: &http.Client{Transport: &http.Transport{
			// Dial the test server whatever host the request names.
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}}}
	if err := s.Put(context.Background(), "openbao-raft/k.snap", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if gotHost != "acme-openbao-backup-e2e."+addr || gotPath != "/openbao-raft/k.snap" {
		t.Errorf("virtual-hosted PUT went to host %q path %q", gotHost, gotPath)
	}
}

func TestStoreErrorsCarryTheS3Code(t *testing.T) {
	_, s := newFake(t)
	s.Bucket = "someone-elses"
	_, err := s.List(context.Background(), Prefix)
	if err == nil || !strings.Contains(err.Error(), "NoSuchBucket") {
		t.Errorf("List on a wrong bucket = %v, want NoSuchBucket named", err)
	}
	if _, err := (Store{}).List(context.Background(), Prefix); err == nil {
		t.Error("an unconfigured store must refuse rather than send unsigned requests")
	}
}

func TestStoreWaitUsableRidesOutKeyPropagation(t *testing.T) {
	f, s := newFake(t)
	f.denyN = 3
	if err := s.WaitUsable(context.Background(), 5*time.Second, 10*time.Millisecond); err != nil {
		t.Fatalf("WaitUsable gave up during propagation: %v", err)
	}
	f.denyN = 1 << 20
	err := s.WaitUsable(context.Background(), 50*time.Millisecond, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("WaitUsable past its budget = %v, want AccessDenied named", err)
	}
}

func TestFieldsToStoreNamesEveryMissingField(t *testing.T) {
	_, err := FieldsToStore(map[string]string{"AWS_ACCESS_KEY_ID": "a"})
	if err == nil {
		t.Fatal("FieldsToStore accepted an incomplete secret")
	}
	for _, f := range []string{"AWS_SECRET_ACCESS_KEY", "BUCKET", "ENDPOINT"} {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error does not name %s: %v", f, err)
		}
	}
	s, err := FieldsToStore(map[string]string{"AWS_ACCESS_KEY_ID": "a", "AWS_SECRET_ACCESS_KEY": "b",
		"BUCKET": "c", "ENDPOINT": "us-ord-10.linodeobjects.com"})
	if err != nil || s.Bucket != "c" {
		t.Errorf("FieldsToStore = %+v, %v", s, err)
	}
	if sch, host := s.base(); sch != "https" || host != "us-ord-10.linodeobjects.com" {
		t.Errorf("a bare-host endpoint must mean https: %s://%s", sch, host)
	}
}

// ── raft API ─────────────────────────────────────────────────────────────────

func TestSaveVerifiesAndRestoreClassifiesAForeignSnapshot(t *testing.T) {
	archive := goodArchive(t)
	var restored []byte
	var forced bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sys/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"initialized":true,"sealed":false,"standby":false,"cluster_id":"c-1","cluster_name":"vault-cluster-x"}`)
	})
	mux.HandleFunc("/v1/sys/storage/raft/snapshot", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "tok" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"errors":["permission denied"]}`)
			return
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write(archive)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errors":["failed to read snapshot file: failed to open the sealed hashes: message authentication failed"]}`)
	})
	mux.HandleFunc("/v1/sys/storage/raft/snapshot-force", func(w http.ResponseWriter, r *http.Request) {
		restored, _ = io.ReadAll(r.Body)
		forced = true
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx := context.Background()

	h, err := ReadHealth(ctx, srv.Client(), srv.URL)
	if err != nil || h.ClusterID != "c-1" || h.Sealed {
		t.Fatalf("ReadHealth = %+v, %v", h, err)
	}
	data, m, err := Save(ctx, srv.Client(), srv.URL, "tok")
	if err != nil || m.Index != 42 || !bytes.Equal(data, archive) {
		t.Fatalf("Save = %d bytes, %+v, %v", len(data), m, err)
	}
	if _, _, err := Save(ctx, srv.Client(), srv.URL, "wrong"); err == nil || !strings.Contains(err.Error(), "openbao-backup policy") {
		t.Errorf("a 403 on save must point at the policy: %v", err)
	}
	err = Restore(ctx, srv.Client(), srv.URL, "tok", data, false)
	if !errors.Is(err, ErrForeignSnapshot) {
		t.Errorf("non-forced restore of a foreign snapshot = %v, want ErrForeignSnapshot", err)
	}
	if err := Restore(ctx, srv.Client(), srv.URL, "tok", data, true); err != nil || !forced || !bytes.Equal(restored, data) {
		t.Errorf("forced restore = %v (forced=%v, %d bytes)", err, forced, len(restored))
	}
}

func TestSaveRefusesAnUnverifiableSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html>proxy error page</html>")
	}))
	defer srv.Close()
	if _, _, err := Save(context.Background(), srv.Client(), srv.URL, "tok"); err == nil {
		t.Error("Save accepted a 200 whose body is not a snapshot — that is what would have been uploaded")
	}
}
