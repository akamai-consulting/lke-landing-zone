package linode

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestMergeTags(t *testing.T) {
	cases := []struct {
		name    string
		tags    []string
		desired []string
		want    []string
		changed bool
	}{
		{"adds all missing", nil, []string{"block-storage", "lke1"}, []string{"block-storage", "lke1"}, true},
		{"adds only missing", []string{"block-storage"}, []string{"block-storage", "lke1"}, []string{"block-storage", "lke1"}, true},
		{"all present is noop", []string{"block-storage", "lke1"}, []string{"block-storage", "lke1"}, []string{"block-storage", "lke1"}, false},
		{"extra existing kept", []string{"custom", "lke1"}, []string{"lke1"}, []string{"custom", "lke1"}, false},
		{"empty desired entries skipped", []string{"a"}, []string{"", "a"}, []string{"a"}, false},
		{"empty desired is noop", []string{"a"}, nil, []string{"a"}, false},
	}
	for _, c := range cases {
		got, changed := MergeTags(c.tags, c.desired)
		if changed != c.changed || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: MergeTags(%v,%v) = %v,%v want %v,%v", c.name, c.tags, c.desired, got, changed, c.want, c.changed)
		}
	}
	// The caller's slice must never be mutated by a merge that appends.
	orig := []string{"block-storage"}
	merged, _ := MergeTags(orig, []string{"lke9"})
	if len(orig) != 1 || orig[0] != "block-storage" {
		t.Errorf("MergeTags mutated its input: %v", orig)
	}
	if len(merged) != 2 {
		t.Errorf("merged = %v, want 2 entries", merged)
	}
}

func TestVolumeAndUpdateVolume(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v4/volumes/404":
			w.WriteHeader(404)
		case r.Method == http.MethodGet && r.URL.Path == "/v4/volumes/1":
			writeJSON(w, 200, map[string]any{"id": 1, "label": "pvc-x", "tags": []string{"block-storage"}})
		case r.Method == http.MethodPut && r.URL.Path == "/v4/volumes/1":
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			writeJSON(w, 200, map[string]any{"id": 1})
		default:
			w.WriteHeader(500)
		}
	})
	ctx := context.Background()

	if _, status, err := c.Volume(ctx, "404"); err != nil || status != 404 {
		t.Errorf("Volume(404) = status %d err %v, want 404/nil", status, err)
	}
	vol, status, err := c.Volume(ctx, "1")
	if err != nil || status != 200 || MapString(vol, "label") != "pvc-x" {
		t.Fatalf("Volume(1) = %v status %d err %v", vol, status, err)
	}
	if status, err := c.UpdateVolume(ctx, "1", "pvc-x", []string{"block-storage", "lke99"}); err != nil || status != 200 {
		t.Fatalf("UpdateVolume = status %d err %v", status, err)
	}
	if gotMethod != http.MethodPut || gotPath != "/v4/volumes/1" {
		t.Errorf("last call = %s %s, want PUT /v4/volumes/1", gotMethod, gotPath)
	}
	// Label passes through unchanged — the reconciler never renames Volumes.
	if gotBody["label"] != "pvc-x" {
		t.Errorf("PUT body label = %v, want pvc-x (unchanged)", gotBody["label"])
	}
}
