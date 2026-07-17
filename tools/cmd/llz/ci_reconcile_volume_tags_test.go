package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestParsePVVolumes(t *testing.T) {
	pvJSON := []byte(`{"items":[
	  {"spec":{"csi":{"driver":"linodebs.csi.linode.com","volumeHandle":"111-pvc-aaa"},
	           "claimRef":{"namespace":"gitea","name":"data-gitea-0"}}},
	  {"spec":{"csi":{"driver":"linodebs.csi.linode.com","volumeHandle":"222-pvc-bbb"}}},
	  {"spec":{"csi":{"driver":"other.csi.io","volumeHandle":"333-x"},
	           "claimRef":{"namespace":"x","name":"y"}}},
	  {"spec":{"csi":{"driver":"linodebs.csi.linode.com","volumeHandle":""}}}
	]}`)
	pvs, err := parsePVVolumes(pvJSON)
	if err != nil {
		t.Fatal(err)
	}
	// Both linodebs PVs parse — including 222, which has NO claimRef (released/
	// unbound PVs are still healed, unlike the retired labeler). Other drivers
	// and empty handles are skipped.
	if len(pvs) != 2 || pvs[0].VolumeID != "111" || pvs[1].VolumeID != "222" {
		t.Fatalf("parsePVVolumes = %+v, want ids 111 + 222", pvs)
	}
	if pvs[0].Namespace != "gitea" || pvs[0].PVC != "data-gitea-0" {
		t.Errorf("claimRef not carried through: %+v", pvs[0])
	}
}

func TestDesiredTagsFromSC(t *testing.T) {
	good := []byte(`{"parameters":{"linodebs.csi.linode.com/volumeTags":"block-storage, platform-support-services,lke123"}}`)
	tags, err := desiredTagsFromSC(good, "block-storage-retain")
	if err != nil || len(tags) != 3 || tags[2] != "lke123" {
		t.Fatalf("desiredTagsFromSC = %v, %v", tags, err)
	}
	// Missing/empty volumeTags is a hard error — healing to an empty set would
	// be a destructive no-op that masks a broken class.
	for _, bad := range []string{`{"parameters":{}}`, `{"parameters":{"linodebs.csi.linode.com/volumeTags":" , "}}`} {
		if _, err := desiredTagsFromSC([]byte(bad), "block-storage-retain"); err == nil {
			t.Errorf("desiredTagsFromSC(%s) should error", bad)
		}
	}
}

// fakeTagClient serves canned volumes and records PUTs.
type fakeTagClient struct {
	vols map[string]map[string]any // id -> volume (nil entry = 404)
	all  []map[string]any          // ListVolumes response
	puts map[string][]string       // id -> tags PUT
}

func (f *fakeTagClient) Volume(_ context.Context, id string) (map[string]any, int, error) {
	v, ok := f.vols[id]
	if !ok || v == nil {
		return nil, 404, nil
	}
	return v, 200, nil
}
func (f *fakeTagClient) UpdateVolume(_ context.Context, id, label string, tags []string) (int, error) {
	if f.puts == nil {
		f.puts = map[string][]string{}
	}
	f.puts[id] = tags
	return 200, nil
}
func (f *fakeTagClient) ListVolumes(context.Context) ([]map[string]any, error) { return f.all, nil }

func tagVol(id, label string, tags ...string) map[string]any {
	t := make([]any, len(tags))
	for i, s := range tags {
		t[i] = s
	}
	return map[string]any{"id": float64(atoiOr0(id)), "label": label, "tags": t}
}

func atoiOr0(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

func TestReconcileVolumeTags(t *testing.T) {
	desired := []string{"block-storage", "platform-support-services", "lke123"}
	f := &fakeTagClient{vols: map[string]map[string]any{
		"1": tagVol("1", "pvc-full", "block-storage", "platform-support-services", "lke123"), // already ok
		"2": tagVol("2", "pvc-clone"),                                                        // untagged clone slip -> heal all
		"3": tagVol("3", "pvc-partial", "block-storage"),                                     // partial -> heal missing
		"4": nil,                                                                             // 404
	}}
	pvs := []pvVolume{{VolumeID: "1"}, {VolumeID: "2", Namespace: "ns", PVC: "c"}, {VolumeID: "3"}, {VolumeID: "4"}}
	r := reconcileVolumeTags(context.Background(), f, desired, pvs, func(string, ...any) {})
	if r.healed != 2 || r.ok != 1 || r.missing != 1 || r.errors != 0 {
		t.Fatalf("result = %+v, want healed=2 ok=1 missing=1", r)
	}
	if got := strings.Join(f.puts["2"], ","); got != "block-storage,platform-support-services,lke123" {
		t.Errorf("clone heal PUT = %q", got)
	}
	if got := strings.Join(f.puts["3"], ","); got != "block-storage,platform-support-services,lke123" {
		t.Errorf("partial heal PUT = %q (existing first, missing appended)", got)
	}
	if _, ok := f.puts["1"]; ok {
		t.Error("already-ok volume must not be PUT")
	}
}

func TestReportAbandonedVolumes(t *testing.T) {
	f := &fakeTagClient{all: []map[string]any{
		tagVol("1", "pvc-live", "lke123"),      // in PV set -> not abandoned
		tagVol("5", "pvc-orphan", "lke123"),    // tagged, no PV -> ABANDONED
		tagVol("6", "pvc-other", "lke999"),     // other cluster -> skip
		tagVol("7", "manual-disk", "lke123"),   // not pvc-* -> skip
		tagVol("8", "pvc-untagged", "kubelet"), // no lke tag -> skip (census's job)
	}}
	var lines []string
	n, err := reportAbandonedVolumes(context.Background(), f, "lke123",
		[]pvVolume{{VolumeID: "1"}}, func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) })
	if err != nil || n != 1 {
		t.Fatalf("abandoned = %d err %v, want 1", n, err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "pvc-orphan") || !strings.Contains(lines[0], "--volume-ids 5") {
		t.Errorf("report line = %v", lines)
	}
}
