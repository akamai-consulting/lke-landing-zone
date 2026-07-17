package main

import (
	"context"
	"testing"
	"time"
)

// TestEnvObjKeyLabelsMatchRotationTable guards that the per-env obj-key reaper
// targets EXACTLY the labels mint-bootstrap-objkeys creates — so a rename of a
// minted key label can never silently leak keys past the account's 100-key cap.
func TestEnvObjKeyLabelsMatchRotationTable(t *testing.T) {
	const env = "e2e"
	reaped := map[string]bool{}
	for _, l := range envObjKeyLabels(env) {
		reaped[l] = true
	}
	minted := 0
	for _, e := range buildRotationTable(env, "us-ord-1") {
		if e.kind != credKindObjKey {
			continue
		}
		minted++
		if !reaped[e.label] {
			t.Errorf("reapEnvObjKeys does not target minted obj-key label %q — it would leak on teardown", e.label)
		}
	}
	if minted == 0 {
		t.Fatal("buildRotationTable produced no obj-key entries — test can't verify coverage")
	}
	// And the reaper must not target a label nothing mints (over-broad delete).
	for l := range reaped {
		found := false
		for _, e := range buildRotationTable(env, "us-ord-1") {
			if e.kind == credKindObjKey && e.label == l {
				found = true
			}
		}
		if !found {
			t.Errorf("reapEnvObjKeys targets label %q that buildRotationTable never mints", l)
		}
	}
}

// TestEnvInclusterPATLabel pins the in-cluster PAT label the reaper deletes to the
// one inclusterPATLabel mints.
func TestEnvInclusterPATLabel(t *testing.T) {
	if got := inclusterPATLabel("e2e"); got != "llz-incluster-e2e" {
		t.Errorf("inclusterPATLabel(e2e) = %q, want llz-incluster-e2e (reaper matches this exactly)", got)
	}
}

// fakeVolReapClient serves a canned live-cluster set + Volume list to reapVolumes.
type fakeVolReapClient struct {
	live    map[string]bool
	vols    []map[string]any
	liveHit int // times LiveClusterIDs was called (0 on the --volume-ids path)
}

func (f *fakeVolReapClient) LiveClusterIDs(context.Context) (map[string]bool, error) {
	f.liveHit++
	return f.live, nil
}
func (f *fakeVolReapClient) ListVolumes(context.Context) ([]map[string]any, error) {
	return f.vols, nil
}

// vol builds a Volume map shaped like the Linode API decode (unattached =>
// linode_id null; tags as []any, id as float64) — see internal/linode accessors.
func vol(id, label, region string, tags ...string) map[string]any {
	t := make([]any, len(tags))
	for i, s := range tags {
		t[i] = s
	}
	return map[string]any{"id": float64(mustAtoi(id)), "label": label, "region": region, "tags": t, "linode_id": nil}
}

func mustAtoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

// captureDel records the volume ids a reap run would delete.
func captureDel(deleted *[]string) func(path, desc string) {
	return func(path, desc string) {
		*deleted = append(*deleted, path)
	}
}

// Region sweep: keep a live cluster's detached PVC, reap a gone cluster's, and
// (fail-safe default) KEEP an untagged one — and confirm the liveness gate was
// consulted. An untagged Volume has no ownership signal, so without --reap-untagged
// it is never reaped, guarding against a coverage gap deleting a live Volume.
func TestReapVolumes_RegionSweepAppliesGate(t *testing.T) {
	f := &fakeVolReapClient{
		live: map[string]bool{"100": true},
		vols: []map[string]any{
			vol("1", "pvc-live", "us-ord", "block-storage", "lke100"),  // live cluster -> KEEP
			vol("2", "pvc-gone", "us-ord", "block-storage", "lke200"),  // gone cluster -> reap
			vol("3", "pvc-legacy", "us-ord", "block-storage"),          // untagged -> KEEP (fail-safe)
			vol("4", "pvc-other", "us-lax", "block-storage", "lke200"), // out of region -> skip
			vol("5", "notpvc", "us-ord", "block-storage"),              // not pvc- -> skip
		},
	}
	var deleted []string
	if err := reapVolumes(context.Background(), f, reapOpts{region: "us-ord"}, captureDel(&deleted)); err != nil {
		t.Fatal(err)
	}
	if f.liveHit != 1 {
		t.Errorf("LiveClusterIDs called %d times, want 1 (region sweep must load the live set)", f.liveHit)
	}
	assertDeleted(t, deleted, "/v4/volumes/2")
}

// Fail-safe default: an untagged pvc- Volume is KEPT regardless of age — only a
// Volume tagged to a GONE cluster is a definitive orphan. Without an ownership
// tag we cannot prove the Volume isn't a live cluster's, so we never reap it by
// default (a coverage gap must not cause data loss).
func TestReapVolumes_UntaggedKeptByDefault(t *testing.T) {
	young := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	f := &fakeVolReapClient{
		live: map[string]bool{"100": true},
		vols: []map[string]any{
			withCreated(vol("1", "pvc-fresh", "us-ord", "block-storage"), young),          // untagged + young -> KEEP
			withCreated(vol("2", "pvc-stale", "us-ord", "block-storage"), old),            // untagged + old   -> KEEP (fail-safe)
			withCreated(vol("3", "pvc-gone", "us-ord", "block-storage", "lke200"), young), // gone cluster     -> reap
		},
	}
	var deleted []string
	if err := reapVolumes(context.Background(), f, reapOpts{region: "us-ord"}, captureDel(&deleted)); err != nil {
		t.Fatal(err)
	}
	assertDeleted(t, deleted, "/v4/volumes/3")
}

// --cluster-label reap: resolved to an lke<id> tag, it reaps ONLY that cluster's
// Volumes and BYPASSES the liveness gate — the caller named the cluster on purpose
// (it's being torn down), so its Volume is reaped even while the live set still
// lists the cluster, and the live set is never loaded.
func TestReapVolumes_ClusterLabelReapsByTagBypassingGate(t *testing.T) {
	f := &fakeVolReapClient{
		live: map[string]bool{"200": true, "100": true}, // 200 still "live" — deliberately targeted anyway
		vols: []map[string]any{
			vol("1", "pvc-target", "us-ord", "block-storage", "lke200"),  // named cluster's Volume -> REAP (gate bypassed)
			vol("2", "pvc-other", "us-ord", "block-storage", "lke100"),   // a different cluster -> not in tag set -> skip
			vol("3", "pvc-untagged", "us-ord", "block-storage"),          // untagged -> not in tag set -> skip
			vol("4", "pvc-target2", "us-lax", "block-storage", "lke200"), // right tag, wrong region -> skip
		},
	}
	var deleted []string
	if err := reapVolumes(context.Background(), f, reapOpts{region: "us-ord", clusterTags: []string{"lke200"}}, captureDel(&deleted)); err != nil {
		t.Fatal(err)
	}
	if f.liveHit != 0 {
		t.Errorf("LiveClusterIDs called %d times, want 0 (--cluster-label is a deliberate scope that bypasses the gate)", f.liveHit)
	}
	assertDeleted(t, deleted, "/v4/volumes/1")
}

// Account-wide (no region): a gone-cluster-tagged Volume is attributable — its
// lke<id> doesn't resolve to a live cluster — so it's reaped in ANY region. A
// live-cluster Volume and an untagged one are kept. This is how an already-deleted
// cluster's Volumes reap by their durable tag without a --region scope.
func TestReapVolumes_AccountWideReapsGoneClusterOrphans(t *testing.T) {
	f := &fakeVolReapClient{
		live: map[string]bool{"100": true},
		vols: []map[string]any{
			vol("1", "pvc-gone-ord", "us-ord", "block-storage", "lke200"), // gone cluster, us-ord -> reap
			vol("2", "pvc-gone-lax", "us-lax", "block-storage", "lke300"), // gone cluster, us-lax -> reap (account-wide)
			vol("3", "pvc-live", "us-ord", "block-storage", "lke100"),     // live cluster -> keep
			vol("4", "pvc-untagged", "us-lax", "block-storage"),           // untagged -> keep (fail-safe)
		},
	}
	var deleted []string
	// No region, no volume-ids, no cluster-tags: the account-wide gone-cluster gate.
	if err := reapVolumes(context.Background(), f, reapOpts{}, captureDel(&deleted)); err != nil {
		t.Fatal(err)
	}
	if f.liveHit != 1 {
		t.Errorf("LiveClusterIDs called %d times, want 1 (account-wide sweep must load the live set to attribute)", f.liveHit)
	}
	assertDeleted(t, deleted, "/v4/volumes/1", "/v4/volumes/2")
}

// --volume-ids allowlist is a deliberate, precise scope: it must BYPASS the
// liveness gate (so CI can tear down a live cluster's Volumes) and must never load
// the live set. Both allowlisted ids delete regardless of cluster-liveness tag.
func TestReapVolumes_VolumeIDsBypassGate(t *testing.T) {
	f := &fakeVolReapClient{
		live: map[string]bool{"100": true}, // would KEEP vol 1 if the gate applied
		vols: []map[string]any{
			vol("1", "pvc-live", "us-ord", "block-storage", "lke100"),
			vol("2", "pvc-gone", "us-ord", "block-storage", "lke200"),
			vol("3", "pvc-unlisted", "us-ord", "block-storage", "lke100"),
		},
	}
	var deleted []string
	if err := reapVolumes(context.Background(), f, reapOpts{volumeIDs: "1 2"}, captureDel(&deleted)); err != nil {
		t.Fatal(err)
	}
	if f.liveHit != 0 {
		t.Errorf("LiveClusterIDs called %d times, want 0 (allowlist path must not load the live set)", f.liveHit)
	}
	// vol 1 is tagged to a LIVE cluster yet still deleted — the allowlist bypasses
	// the gate; vol 3 is not on the allowlist so it is untouched.
	assertDeleted(t, deleted, "/v4/volumes/1", "/v4/volumes/2")
}

// --reap-untagged opts into reaping untagged Volumes, but the age guard still
// spares one younger than the grace window (may be a live cluster's Volume awaiting
// its provision-time tag); an old untagged one is reaped. Uses relative timestamps
// against reapVolumes' own time.Now().
func TestReapVolumes_ReapUntaggedRespectsAgeGuard(t *testing.T) {
	young := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	f := &fakeVolReapClient{
		live: map[string]bool{"100": true},
		vols: []map[string]any{
			withCreated(vol("1", "pvc-fresh", "us-ord", "block-storage"), young),          // untagged + young -> KEEP (grace)
			withCreated(vol("2", "pvc-stale", "us-ord", "block-storage"), old),            // untagged + old   -> reap (opted in)
			withCreated(vol("3", "pvc-gone", "us-ord", "block-storage", "lke200"), young), // gone cluster: age guard N/A -> reap
		},
	}
	var deleted []string
	if err := reapVolumes(context.Background(), f, reapOpts{region: "us-ord", reapUntagged: true}, captureDel(&deleted)); err != nil {
		t.Fatal(err)
	}
	assertDeleted(t, deleted, "/v4/volumes/2", "/v4/volumes/3")
}

func withCreated(v map[string]any, created string) map[string]any {
	v["created"] = created
	return v
}

func assertDeleted(t *testing.T, got []string, want ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	if len(got) != len(want) {
		t.Fatalf("deleted %v, want exactly %v", got, want)
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("expected %s to be deleted; got %v", w, got)
		}
	}
}
