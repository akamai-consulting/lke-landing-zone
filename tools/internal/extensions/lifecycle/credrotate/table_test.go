package credrotate

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

func strconvI(n int64) string { return strconv.FormatInt(n, 10) }

// jn mirrors how the Linode client decodes ids — json.Number, the only type
// cli.AsUint64 accepts.
func jn(i int) json.Number { return json.Number(strconv.Itoa(i)) }

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
		if got := IsDue(tc.rotatedAt, now, tc.after); got != tc.want {
			t.Errorf("%s: IsDue=%v, want %v", tc.name, got, tc.want)
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
		got := IDsToDrain(append([]uint64(nil), tc.ids...), tc.keep)
		if len(got) != len(tc.want) {
			t.Errorf("%s: IDsToDrain=%v, want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: IDsToDrain=%v, want %v", tc.name, got, tc.want)
				break
			}
		}
	}
}

func TestIdsByLabel(t *testing.T) {
	items := []map[string]any{
		{"id": jn(1), "label": "acme-loki-primary"},
		{"id": jn(2), "label": "other"},
		{"id": jn(3), "label": "acme-loki-primary"},
		{"label": "acme-loki-primary"}, // no id -> skipped
	}
	got := idsByLabel(items, "acme-loki-primary")
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("idsByLabel = %v, want [1 3]", got)
	}
}

func TestBuildRotationTable(t *testing.T) {
	table := BuildRotationTable("acme", "primary", "us-ord-1")
	if len(table) != 1 {
		t.Fatalf("table has %d entries, want 1 (obj-platform; the two per-app keys were retired)", len(table))
	}
	byName := map[string]CredEntry{}
	for _, e := range table {
		byName[e.Name] = e
	}

	// THE TWO PER-APP ENTRIES ARE GONE, and this test used to assert their bucket
	// scopes and field sets in detail — correctly, about credentials nothing
	// consumed. The Loki key's read_write grant spanned chunks/ruler/admin (which
	// hold the OpenBao audit stream) and its ExternalSecret had been deleted by
	// 52465691. A precise test over a dead credential is still a dead credential.
	//
	// Their absence is asserted rather than merely implied by the count above: a
	// re-add would otherwise only trip the length check, which reads as an
	// off-by-one rather than as the retirement being undone.
	for _, gone := range []string{"loki-object-store", "harbor-registry-s3"} {
		if _, back := byName[gone]; back {
			t.Errorf("%s is in the rotation table again — it has no consumer (its ExternalSecret "+
				"was deleted when object storage went apl-core-native), so minting and rotating it "+
				"writes a read_write Linode key into a path nothing reads. See credpaths.go", gone)
		}
	}

	// The broad managed platform-obj key: seeded at secret/obj/platform, scoped to
	// every provisioned bucket (loki chunks/ruler/admin + harbor), AWS_* fields.
	obj := byName["obj-platform"]
	wantObjBuckets := "acme-loki-chunks-primary,acme-loki-ruler-primary,acme-loki-admin-primary,acme-harbor-registry-primary"
	if obj.Kind != CredKindObjKey || obj.BaoPath != "secret/obj/platform" || strings.Join(obj.Buckets, ",") != wantObjBuckets {
		t.Errorf("obj-platform entry = %+v (want buckets %s)", obj, wantObjBuckets)
	}
	if of := obj.Fields("AK", "SK"); of["AWS_ACCESS_KEY_ID"] != "AK" || of["AWS_SECRET_ACCESS_KEY"] != "SK" {
		t.Errorf("obj-platform fields = %v", of)
	}
}

// ── orchestration (stubbed deps) ─────────────────────────────────────────────

type stubLinode struct {
	pats, objkeys []map[string]any
	deleted       []uint64
	verifyErr     error
	patCreates    int
	objCreates    int
	// listErr makes the key listing fail, so "the grant is wrong" and "the grant
	// could not be read" can be exercised as different states.
	listErr error
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
	return s.objkeys, s.listErr
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

func withRotatorStubs(t *testing.T, lc LinodeAPI, bao openbao.BaoStore, now time.Time) {
	t.Helper()
	ol, ob, on := NewLinodeClient, NewBaoStore, Now
	NewLinodeClient = func(string) LinodeAPI { return lc }
	NewBaoStore = func(context.Context) (openbao.BaoStore, error) { return bao, nil }
	Now = func() time.Time { return now }
	t.Cleanup(func() { NewLinodeClient, NewBaoStore, Now = ol, ob, on })
}

func TestRunRotateLinodeCreds(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	t.Setenv("REGION", "primary")
	t.Setenv("OBJ_CLUSTER", "us-ord-1")
	// The in-cluster rotator has no spec to read; `llz render` writes this into
	// the reconciler Deployment (RenderReconcilerEnvPatch).
	t.Setenv("OBJ_LABEL_PREFIX", "acme")
	t.Setenv("LINODE_TOKEN", "minting")

	t.Run("all due -> mint+write both, drain old", func(t *testing.T) {
		lc := &stubLinode{
			// pre-existing older resources to drain (keep-newest default 2)
			// Three old keys under the label the rotator still mints, so the
			// keep-newest drain has something to delete. (These were
			// "acme-loki-primary" until that per-app key was retired.)
			objkeys: []map[string]any{{"id": jn(10), "label": "acme-obj-primary"}, {"id": jn(11), "label": "acme-obj-primary"}, {"id": jn(201), "label": "acme-obj-primary"}},
		}
		bao := &stubBao{data: map[string]map[string]string{}} // empty -> all due
		withRotatorStubs(t, lc, bao, now)
		if err := RunRotateLinodeCreds(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		if bao.data["secret/obj/platform"]["rotated_at"] == "" {
			t.Errorf("secret/obj/platform not written with rotated_at: %v", bao.data["secret/obj/platform"])
		}
		if bao.data["secret/obj/platform"]["AWS_ACCESS_KEY_ID"] != "AK" {
			t.Errorf("obj-platform key not written: %v", bao.data["secret/obj/platform"])
		}
		// The retired per-app paths must not be written again: the rotator writes
		// what the table declares, and a write here would mean the entries came back.
		for _, gone := range []string{"secret/loki/object-store", "secret/harbor/registry-s3"} {
			if len(bao.data[gone]) != 0 {
				t.Errorf("rotator wrote retired path %s: %v", gone, bao.data[gone])
			}
		}
		if len(lc.deleted) == 0 {
			t.Error("expected old resources to be drained")
		}
	})

	t.Run("not due -> no mint, no write", func(t *testing.T) {
		recent := strconvI(now.Unix() - 1*86400)
		lc := &stubLinode{}
		bao := &stubBao{data: map[string]map[string]string{
			// The retired paths are present and recent, which is the state an
			// instance bootstrapped before the retirement is actually in — the
			// rotator must ignore them because the TABLE no longer names them, not
			// because they look fresh.
			"secret/loki/object-store":  {"rotated_at": recent},
			"secret/harbor/registry-s3": {"rotated_at": recent},
			"secret/obj/platform":       {"rotated_at": recent},
		}}
		withRotatorStubs(t, lc, bao, now)
		if err := RunRotateLinodeCreds(context.Background(), true); err != nil {
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
		if err := RunRotateLinodeCreds(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		if lc.patCreates != 0 || lc.objCreates != 0 || len(bao.data) != 0 {
			t.Errorf("dry-run must not mint/write (pat=%d obj=%d writes=%d)", lc.patCreates, lc.objCreates, len(bao.data))
		}
	})

}

// THE 42-DAY WINDOW, as a test. IsDue asks only how old the credential is, so a
// key that cannot write a single byte reads as healthy for the whole rotation
// period — and this rotator is what owns object-storage keys after first boot,
// so nothing else was going to notice. Measured on a production instance: 403
// AccessDenied on every write for 42 days while the rotator ticked daily and
// reported "not due" each time, with ROTATE_AFTER_DAYS defaulting to 80.
func TestRotatorReplacesAKeyThatCannotWriteEvenWhenNotDueByAge(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	t.Setenv("REGION", "primary")
	t.Setenv("OBJ_CLUSTER", "us-ord-1")
	t.Setenv("OBJ_LABEL_PREFIX", "acme")
	t.Setenv("LINODE_TOKEN", "minting")
	recent := strconvI(now.Unix() - 1*86400)

	// Scoped to somebody else's buckets — what a key minted under a different
	// objLabelPrefix looks like, which is the shape that produced the outage.
	stale := []map[string]any{{
		"id": jn(10), "label": "acme-obj-primary", "access_key": "OLD",
		"bucket_access": []any{
			map[string]any{"bucket_name": "platform-loki-chunks-primary", "permissions": "read_write"},
		},
	}}

	t.Run("cannot write -> rotated despite being fresh", func(t *testing.T) {
		lc := &stubLinode{objkeys: stale}
		bao := &stubBao{data: map[string]map[string]string{
			"secret/obj/platform": {"rotated_at": recent, "AWS_ACCESS_KEY_ID": "OLD"},
		}}
		withRotatorStubs(t, lc, bao, now)
		if err := RunRotateLinodeCreds(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		if lc.objCreates != 1 {
			t.Errorf("objCreates=%d, want 1 — a key that cannot write must not wait out the "+
				"age threshold", lc.objCreates)
		}
		if bao.data["secret/obj/platform"]["AWS_ACCESS_KEY_ID"] != "AK" {
			t.Errorf("the replacement was not written: %v", bao.data["secret/obj/platform"])
		}
	})

	// CONTROL. A fresh key that DOES grant its buckets must still be left alone,
	// or the rotator mints on every tick and burns the account key cap.
	t.Run("still grants -> untouched", func(t *testing.T) {
		lc := &stubLinode{objkeys: []map[string]any{{
			"id": jn(10), "label": "acme-obj-primary", "access_key": "OLD",
			"bucket_access": []any{
				map[string]any{"bucket_name": "acme-loki-chunks-primary", "permissions": "read_write"},
				map[string]any{"bucket_name": "acme-loki-ruler-primary", "permissions": "read_write"},
				map[string]any{"bucket_name": "acme-loki-admin-primary", "permissions": "read_write"},
				map[string]any{"bucket_name": "acme-harbor-registry-primary", "permissions": "read_write"},
			},
		}}}
		bao := &stubBao{data: map[string]map[string]string{
			"secret/obj/platform": {"rotated_at": recent, "AWS_ACCESS_KEY_ID": "OLD"},
		}}
		withRotatorStubs(t, lc, bao, now)
		if err := RunRotateLinodeCreds(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		if lc.objCreates != 0 {
			t.Errorf("objCreates=%d, want 0 — a healthy key must not be rotated off-schedule", lc.objCreates)
		}
	})

	// FAIL-CLOSED AGAINST CHURN, which is the opposite direction from the check
	// itself. Rotating costs a mint against the account key cap and starts a
	// drain, so an unreadable listing must keep the old behaviour rather than
	// treat "cannot tell" as "broken".
	t.Run("cannot tell -> untouched", func(t *testing.T) {
		lc := &stubLinode{listErr: errors.New("connection reset")}
		bao := &stubBao{data: map[string]map[string]string{
			"secret/obj/platform": {"rotated_at": recent, "AWS_ACCESS_KEY_ID": "OLD"},
		}}
		withRotatorStubs(t, lc, bao, now)
		if err := RunRotateLinodeCreds(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		if lc.objCreates != 0 {
			t.Errorf("objCreates=%d, want 0 — an unverifiable grant must not trigger a rotation",
				lc.objCreates)
		}
	})

	// A dry run reports without minting, same as the age path.
	t.Run("dry-run -> reported, not minted", func(t *testing.T) {
		lc := &stubLinode{objkeys: stale}
		bao := &stubBao{data: map[string]map[string]string{
			"secret/obj/platform": {"rotated_at": recent, "AWS_ACCESS_KEY_ID": "OLD"},
		}}
		withRotatorStubs(t, lc, bao, now)
		if err := RunRotateLinodeCreds(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		if lc.objCreates != 0 {
			t.Errorf("dry-run minted %d key(s)", lc.objCreates)
		}
	})
}
