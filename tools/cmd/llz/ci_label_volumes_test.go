package main

import (
	"context"
	"errors"
	"testing"
)

func TestParsePVVolumes(t *testing.T) {
	js := []byte(`{"items":[
	  {"spec":{"csi":{"driver":"linodebs.csi.linode.com","volumeHandle":"12345-pvc-abc"},"claimRef":{"namespace":"gitea","name":"data-gitea-0"}}},
	  {"spec":{"csi":{"driver":"other.csi","volumeHandle":"9-x"},"claimRef":{"namespace":"x","name":"y"}}},
	  {"spec":{"csi":{"driver":"linodebs.csi.linode.com","volumeHandle":"77-pvc-z"}}},
	  {"spec":{"csi":{"driver":"linodebs.csi.linode.com","volumeHandle":"88-pvc-q"},"claimRef":{"namespace":"mon","name":"prom-0"}}}
	]}`)
	got, err := parsePVVolumes(js)
	if err != nil {
		t.Fatal(err)
	}
	want := []pvVolume{
		{VolumeID: "12345", Namespace: "gitea", PVC: "data-gitea-0"},
		{VolumeID: "88", Namespace: "mon", PVC: "prom-0"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d pvs, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pv[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if _, err := parsePVVolumes([]byte("not json")); err == nil {
		t.Error("expected parse error on bad json")
	}
}

func TestFirstNodeInstanceID(t *testing.T) {
	js := []byte(`{"items":[
	  {"spec":{"providerID":""}},
	  {"spec":{"providerID":"linode://616722"}}
	]}`)
	got, err := firstNodeInstanceID(js)
	if err != nil || got != "616722" {
		t.Fatalf("firstNodeInstanceID = %q err %v, want 616722", got, err)
	}
	none, err := firstNodeInstanceID([]byte(`{"items":[{"spec":{"providerID":"aws://x"}}]}`))
	if err != nil || none != "" {
		t.Errorf("non-linode providerID = %q, want empty", none)
	}
}

// fakeVolClient implements volumeClient from canned per-id state.
type fakeVolClient struct {
	vols   map[string]map[string]any // id -> volume (nil entry => 404)
	puts   map[string][]string       // id -> tags last PUT
	putErr map[string]error
}

func (f *fakeVolClient) Volume(_ context.Context, id string) (map[string]any, int, error) {
	v, ok := f.vols[id]
	if !ok {
		return nil, 500, errors.New("unknown")
	}
	if v == nil {
		return nil, 404, nil
	}
	return v, 200, nil
}

func (f *fakeVolClient) UpdateVolume(_ context.Context, id, label string, tags []string) (int, error) {
	if err := f.putErr[id]; err != nil {
		return 500, err
	}
	if f.puts == nil {
		f.puts = map[string][]string{}
	}
	f.puts[id] = tags
	return 200, nil
}

func TestReconcileVolumes(t *testing.T) {
	f := &fakeVolClient{
		vols: map[string]map[string]any{
			// needs rename + tag
			"1": {"label": "pvc-uuid1", "tags": []any{"block-storage"}},
			// already correct label + tag -> ok, no PUT
			"2": {"label": "pri-gitea-data-gitea-0", "tags": []any{"block-storage", "lke99"}},
			// correct label, missing tag -> tag-only
			"3": {"label": "pri-mon-prom-0", "tags": []any{"block-storage"}},
			// gone
			"4": nil,
		},
		putErr: map[string]error{},
	}
	pvs := []pvVolume{
		{VolumeID: "1", Namespace: "gitea", PVC: "data-gitea-0"},
		{VolumeID: "2", Namespace: "gitea", PVC: "data-gitea-0"},
		{VolumeID: "3", Namespace: "mon", PVC: "prom-0"},
		{VolumeID: "4", Namespace: "x", PVC: "y"},
	}
	r := reconcileVolumes(context.Background(), f, "pri", "lke99", pvs, func(string, ...any) {})

	if r.renamed != 1 || r.tagged != 2 || r.ok != 1 || r.missing != 1 || r.errors != 0 {
		t.Errorf("result = %+v, want renamed=1 tagged=2 ok=1 missing=1 errors=0", r)
	}
	// vol 1 got both, with cluster tag merged onto existing
	if got := f.puts["1"]; len(got) != 2 || got[0] != "block-storage" || got[1] != "lke99" {
		t.Errorf("vol 1 PUT tags = %v, want [block-storage lke99]", got)
	}
	// vol 2 must NOT have been PUT (already correct)
	if _, put := f.puts["2"]; put {
		t.Error("vol 2 was PUT but should have been skipped")
	}
	// vol 3 tag-only PUT keeps the corrected label
	if _, put := f.puts["3"]; !put {
		t.Error("vol 3 should have been PUT (missing tag)")
	}
}

func TestReconcileVolumes_PutError(t *testing.T) {
	f := &fakeVolClient{
		vols:   map[string]map[string]any{"1": {"label": "old", "tags": []any{}}},
		putErr: map[string]error{"1": errors.New("boom")},
	}
	r := reconcileVolumes(context.Background(), f, "pri", "lke1", []pvVolume{{VolumeID: "1", Namespace: "n", PVC: "p"}}, func(string, ...any) {})
	if r.errors != 1 || r.renamed != 0 || r.tagged != 0 {
		t.Errorf("result = %+v, want errors=1", r)
	}
}

// Empty clusterTag => relabel only, no tag changes counted.
func TestReconcileVolumes_NoClusterTag(t *testing.T) {
	f := &fakeVolClient{vols: map[string]map[string]any{"1": {"label": "old", "tags": []any{"block-storage"}}}}
	r := reconcileVolumes(context.Background(), f, "pri", "", []pvVolume{{VolumeID: "1", Namespace: "n", PVC: "p"}}, func(string, ...any) {})
	if r.renamed != 1 || r.tagged != 0 {
		t.Errorf("result = %+v, want renamed=1 tagged=0", r)
	}
	if got := f.puts["1"]; len(got) != 1 || got[0] != "block-storage" {
		t.Errorf("tags should be unchanged: %v", got)
	}
}
