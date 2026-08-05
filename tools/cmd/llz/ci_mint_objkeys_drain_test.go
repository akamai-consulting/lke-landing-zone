package main

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"testing"
)

// drainFakeAPI is the slice of rotatorLinodeAPI the drain touches. Embedding the
// interface keeps the fake honest without restating twenty unused methods: a
// method the drain starts calling panics rather than silently returning a zero.
type drainFakeAPI struct {
	rotatorLinodeAPI
	keys    []map[string]any
	deleted []uint64
	listErr error
	delErr  error
}

func (f *drainFakeAPI) ListObjectStorageKeys(context.Context) ([]map[string]any, error) {
	return f.keys, f.listErr
}

func (f *drainFakeAPI) DeleteObjectStorageKey(_ context.Context, id uint64) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, id)
	return nil
}

// objKey mirrors a decoded Linode listing entry. The id is a json.Number, not a
// float64: the client decodes with UseNumber and cli.AsUint64 accepts ONLY
// json.Number — a float64 fixture makes every id unreadable, so the drain reads
// as a silent no-op and the test passes for the wrong reason.
func objKey(id uint64, label string) map[string]any {
	return map[string]any{"id": json.Number(strconv.FormatUint(id, 10)), "label": label}
}

func TestDrainKeepsTheRotationOverlap(t *testing.T) {
	// The regression this exists to prevent: the first cut deleted EVERY
	// same-labelled key except the one just minted. On a re-bootstrap against a
	// live cluster (OpenBao path wiped, cluster still serving) that revokes the key
	// every running consumer holds until ESO re-syncs — turning a recovery into an
	// outage. The rotator keeps two generations precisely so a swap has an overlap
	// window; the drain must not collapse it.
	f := &drainFakeAPI{keys: []map[string]any{
		objKey(500, "p-loki-lab"), // freshly minted (highest id == newest)
		objKey(400, "p-loki-lab"), // the previous live key — MUST survive
		objKey(300, "p-loki-lab"), // older orphan
		objKey(200, "p-loki-lab"), // older orphan
	}}
	drainSupersededObjKeys(context.Background(), f, "p-loki-lab", 500)

	sort.Slice(f.deleted, func(i, j int) bool { return f.deleted[i] < f.deleted[j] })
	want := []uint64{200, 300}
	if len(f.deleted) != len(want) {
		t.Fatalf("deleted %v, want %v (the 2 newest are kept for overlap)", f.deleted, want)
	}
	for i := range want {
		if f.deleted[i] != want[i] {
			t.Fatalf("deleted %v, want %v", f.deleted, want)
		}
	}
}

func TestDrainNeverTouchesAnotherLabel(t *testing.T) {
	// Labels are namespaced per instance (objlabels.go). A drain that matched
	// loosely would reach a sibling deployment's — or a sibling INSTANCE's —
	// credentials, which is the whole failure class this work exists to remove.
	f := &drainFakeAPI{keys: []map[string]any{
		objKey(500, "p-loki-lab"),
		objKey(400, "p-loki-lab"),
		objKey(300, "p-loki-lab"),
		objKey(299, "p-harbor-registry-lab"), // different credential, same instance
		objKey(298, "other-loki-lab"),        // different INSTANCE
	}}
	drainSupersededObjKeys(context.Background(), f, "p-loki-lab", 500)

	if len(f.deleted) != 1 || f.deleted[0] != 300 {
		t.Fatalf("deleted %v, want only [300] — nothing outside the label", f.deleted)
	}
}

func TestDrainIsANoOpWithinTheKeepWindow(t *testing.T) {
	// The normal first-install shape: one key, just minted. Nothing to drain, and
	// in particular not the key itself.
	f := &drainFakeAPI{keys: []map[string]any{objKey(500, "p-loki-lab")}}
	drainSupersededObjKeys(context.Background(), f, "p-loki-lab", 500)
	if len(f.deleted) != 0 {
		t.Fatalf("deleted %v on a fresh install; want none", f.deleted)
	}
}

func TestDrainSurvivesAnUnreadableListing(t *testing.T) {
	// Best-effort by construction: the key it protects is already live and seeded,
	// so a failed listing must not fail the bootstrap.
	f := &drainFakeAPI{listErr: errors.New("503")}
	drainSupersededObjKeys(context.Background(), f, "p-loki-lab", 500)
	if len(f.deleted) != 0 {
		t.Fatalf("deleted %v despite an unreadable listing", f.deleted)
	}
}

func TestDrainContinuesPastADeleteFailure(t *testing.T) {
	// One un-deletable key must not strand the rest: the point is to bound
	// accumulation, and stopping at the first error leaves it unbounded.
	f := &drainFakeAPI{
		keys: []map[string]any{
			objKey(500, "p-loki-lab"), objKey(400, "p-loki-lab"),
			objKey(300, "p-loki-lab"), objKey(200, "p-loki-lab"),
		},
		delErr: errors.New("409"),
	}
	drainSupersededObjKeys(context.Background(), f, "p-loki-lab", 500)
	if len(f.deleted) != 0 {
		t.Fatalf("recorded deletes %v despite every call failing", f.deleted)
	}
}
