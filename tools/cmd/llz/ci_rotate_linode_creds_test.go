package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func strconvI(n int64) string { return strconv.FormatInt(n, 10) }

// jn mirrors how the Linode client decodes ids — json.Number, the only type
// cli.AsUint64 accepts.
func jn(i int) json.Number { return json.Number(strconv.Itoa(i)) }

var errStub = errors.New("stub error")

func TestIsDue(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	day := int64(86400)
	for _, tc := range []struct {
		name      string
		rotatedAt string
		after     int
		want      bool
	}{
		{"never rotated (empty) is due", "", 80, true},
		{"unparseable is due", "not-a-ts", 80, true},
		{"recently rotated is not due", strconvI(now.Unix() - 10*day), 80, false},
		{"exactly at threshold is due", strconvI(now.Unix() - 80*day), 80, true},
		{"past threshold is due", strconvI(now.Unix() - 365*day), 80, true},
	} {
		if got := isDue(tc.rotatedAt, now, tc.after); got != tc.want {
			t.Errorf("%s: isDue=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIdsToDrain(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []uint64
		keep int
		want []uint64
	}{
		{"fewer than keep -> none", []uint64{5, 3}, 3, nil},
		{"equal to keep -> none", []uint64{5, 3}, 2, nil},
		{"drains all but newest N (sorted desc)", []uint64{1, 9, 4, 7}, 2, []uint64{4, 1}},
		{"keep floored at 1 (keeps only the newest)", []uint64{9, 4, 7}, 0, []uint64{7, 4}},
		{"single key never drained", []uint64{42}, 2, nil},
	} {
		got := idsToDrain(append([]uint64(nil), tc.ids...), tc.keep)
		if len(got) != len(tc.want) {
			t.Errorf("%s: idsToDrain=%v, want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: idsToDrain=%v, want %v", tc.name, got, tc.want)
				break
			}
		}
	}
}

func TestIdsByLabel(t *testing.T) {
	items := []map[string]any{
		{"id": jn(1), "label": "platform-loki-primary"},
		{"id": jn(2), "label": "other"},
		{"id": jn(3), "label": "platform-loki-primary"},
		{"label": "platform-loki-primary"}, // no id -> skipped
	}
	got := idsByLabel(items, "platform-loki-primary")
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("idsByLabel = %v, want [1 3]", got)
	}
}

func TestBuildRotationTable(t *testing.T) {
	table := buildRotationTable("primary", "us-ord-1")
	if len(table) != 3 {
		t.Fatalf("table has %d entries, want 3", len(table))
	}
	byName := map[string]credEntry{}
	for _, e := range table {
		byName[e.name] = e
	}

	dns := byName["dns-token"]
	if dns.kind != credKindPAT || dns.label != "llz-dns-primary" || dns.baoPath != "secret/certmanager/dns01" {
		t.Errorf("dns entry = %+v", dns)
	}
	if f := dns.fields("tok123", ""); f["token"] != "tok123" || len(f) != 1 {
		t.Errorf("dns fields = %v, want {token: tok123}", f)
	}

	loki := byName["loki-object-store"]
	// The Loki key spans the three REAL bucket names (chunks/ruler/admin) — the
	// grant set the llz-object-storage module's bootstrap key carries. An earlier
	// revision minted against the nonexistent "platform-loki-<region>" bucket.
	wantLokiBuckets := "platform-loki-chunks-primary,platform-loki-ruler-primary,platform-loki-admin-primary"
	if loki.kind != credKindObjKey || strings.Join(loki.buckets, ",") != wantLokiBuckets || loki.objCluster != "us-ord-1" {
		t.Errorf("loki entry = %+v (want buckets %s)", loki, wantLokiBuckets)
	}
	if f := loki.fields("AK", "SK"); f["AWS_ACCESS_KEY_ID"] != "AK" || f["AWS_SECRET_ACCESS_KEY"] != "SK" {
		t.Errorf("loki fields = %v", f)
	}

	harbor := byName["harbor-registry-s3"]
	if harbor.kind != credKindObjKey || harbor.baoPath != "secret/harbor/registry-s3" {
		t.Errorf("harbor entry = %+v", harbor)
	}
	// Harbor rewrites the COMPLETE field set (incl. static bucket/endpoint/region).
	f := harbor.fields("AK", "SK")
	for _, k := range []string{"access_key_id", "secret_access_key", "bucket_name", "endpoint", "region"} {
		if f[k] == "" {
			t.Errorf("harbor fields missing %s: %v", k, f)
		}
	}
}

// ── orchestration (stubbed deps) ─────────────────────────────────────────────

type stubLinode struct {
	pats, objkeys []map[string]any
	deleted       []uint64
	verifyErr     error
	patCreates    int
	objCreates    int
}

func (s *stubLinode) ListProfileTokens(context.Context) ([]map[string]any, error) { return s.pats, nil }
func (s *stubLinode) CreateProfileToken(context.Context, string, string, string) (map[string]any, error) {
	s.patCreates++
	return map[string]any{"id": 100 + s.patCreates, "token": "new-pat"}, nil
}
func (s *stubLinode) DeleteProfileToken(_ context.Context, id uint64) error {
	s.deleted = append(s.deleted, id)
	return nil
}
func (s *stubLinode) ListObjectStorageKeys(context.Context) ([]map[string]any, error) {
	return s.objkeys, nil
}
func (s *stubLinode) CreateObjectStorageKeyBuckets(context.Context, string, string, []string, string) (map[string]any, error) {
	s.objCreates++
	// id as json.Number — the only numeric type cli.AsUint64 accepts, mirroring
	// how the real client decodes API responses.
	return map[string]any{"id": jn(200 + s.objCreates), "access_key": "AK", "secret_key": "SK"}, nil
}
func (s *stubLinode) DeleteObjectStorageKey(_ context.Context, id uint64) error {
	s.deleted = append(s.deleted, id)
	return nil
}
func (s *stubLinode) Verify(context.Context) error { return s.verifyErr }

type stubBao struct{ data map[string]map[string]string }

func (b *stubBao) Get(_ context.Context, path, key string) (string, bool, error) {
	v, ok := b.data[path][key]
	return v, ok, nil
}
func (b *stubBao) Write(_ context.Context, path string, d map[string]string) error {
	b.data[path] = d
	return nil
}

func withRotatorStubs(t *testing.T, lc rotatorLinodeAPI, bao baoStore, now time.Time) {
	t.Helper()
	ol, ob, on := linodeRotatorClient, newRotatorBaoStore, rotatorNow
	linodeRotatorClient = func(string) rotatorLinodeAPI { return lc }
	newRotatorBaoStore = func(context.Context) (baoStore, error) { return bao, nil }
	rotatorNow = func() time.Time { return now }
	t.Cleanup(func() { linodeRotatorClient, newRotatorBaoStore, rotatorNow = ol, ob, on })
}

func TestRunRotateLinodeCreds(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	t.Setenv("REGION", "primary")
	t.Setenv("OBJ_CLUSTER", "us-ord-1")
	t.Setenv("LINODE_TOKEN", "minting")

	t.Run("all due -> mint+write all three, drain old", func(t *testing.T) {
		lc := &stubLinode{
			// pre-existing older resources to drain (keep-newest default 2)
			pats:    []map[string]any{{"id": jn(1), "label": "llz-dns-primary"}, {"id": jn(2), "label": "llz-dns-primary"}, {"id": jn(101), "label": "llz-dns-primary"}},
			objkeys: []map[string]any{{"id": jn(10), "label": "platform-loki-primary"}, {"id": jn(11), "label": "platform-loki-primary"}, {"id": jn(201), "label": "platform-loki-primary"}},
		}
		bao := &stubBao{data: map[string]map[string]string{}} // empty -> all due
		withRotatorStubs(t, lc, bao, now)
		if err := runRotateLinodeCreds(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"secret/certmanager/dns01", "secret/loki/object-store", "secret/harbor/registry-s3"} {
			if bao.data[p]["rotated_at"] == "" {
				t.Errorf("%s not written with rotated_at: %v", p, bao.data[p])
			}
		}
		if bao.data["secret/certmanager/dns01"]["token"] != "new-pat" {
			t.Errorf("dns token not written: %v", bao.data["secret/certmanager/dns01"])
		}
		if bao.data["secret/loki/object-store"]["AWS_ACCESS_KEY_ID"] != "AK" {
			t.Errorf("loki key not written: %v", bao.data["secret/loki/object-store"])
		}
		if len(lc.deleted) == 0 {
			t.Error("expected old resources to be drained")
		}
	})

	t.Run("not due -> no mint, no write", func(t *testing.T) {
		recent := strconvI(now.Unix() - 1*86400)
		lc := &stubLinode{}
		bao := &stubBao{data: map[string]map[string]string{
			"secret/certmanager/dns01":  {"rotated_at": recent},
			"secret/loki/object-store":  {"rotated_at": recent},
			"secret/harbor/registry-s3": {"rotated_at": recent},
		}}
		withRotatorStubs(t, lc, bao, now)
		if err := runRotateLinodeCreds(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		if lc.patCreates != 0 || lc.objCreates != 0 {
			t.Errorf("nothing should be minted when not due (pat=%d obj=%d)", lc.patCreates, lc.objCreates)
		}
	})

	t.Run("dry-run -> no mint even when due", func(t *testing.T) {
		lc := &stubLinode{}
		bao := &stubBao{data: map[string]map[string]string{}}
		withRotatorStubs(t, lc, bao, now)
		if err := runRotateLinodeCreds(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		if lc.patCreates != 0 || lc.objCreates != 0 || len(bao.data) != 0 {
			t.Errorf("dry-run must not mint/write (pat=%d obj=%d writes=%d)", lc.patCreates, lc.objCreates, len(bao.data))
		}
	})

	t.Run("bad new token -> error, old not drained", func(t *testing.T) {
		lc := &stubLinode{verifyErr: errStub}
		bao := &stubBao{data: map[string]map[string]string{}}
		withRotatorStubs(t, lc, bao, now)
		if err := runRotateLinodeCreds(context.Background(), true); err == nil {
			t.Error("expected error when the new token fails verification")
		}
		if len(lc.deleted) != 0 {
			t.Error("must not drain when verification failed")
		}
	})
}
