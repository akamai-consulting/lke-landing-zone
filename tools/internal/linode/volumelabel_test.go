package linode

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestDesiredVolumeLabel(t *testing.T) {
	cases := []struct {
		region, ns, pvc string
		want            string
	}{
		{"pri", "gitea", "data-gitea-0", "pri-gitea-data-gitea-0"},
		// illegal chars -> '-'
		{"pri", "ns.dot", "a/b", "pri-ns-dot-a-b"},
		// truncation to exactly 32 (no trailing dash to strip)
		{"sec", "prometheus", "prometheus-server-data-xyz", "sec-prometheus-prometheus-server"},
		{"sec", "aaaaaaaaaa", "bbbbbbbbbbbbbbbbbbb-cccc", "sec-aaaaaaaaaa-bbbbbbbbbbbbbbbbb"},
		// truncation landing on a '-' strips it: "sec-" + 27 a's = 31, char 32 is
		// the separator '-', which TrimRight removes -> 31 chars.
		{"sec", strings.Repeat("a", 27), "data", "sec-" + strings.Repeat("a", 27)},
	}
	for _, c := range cases {
		got := DesiredVolumeLabel(c.region, c.ns, c.pvc)
		if got != c.want {
			t.Errorf("DesiredVolumeLabel(%q,%q,%q) = %q, want %q", c.region, c.ns, c.pvc, got, c.want)
		}
		if len(got) > 32 {
			t.Errorf("label %q exceeds 32 chars", got)
		}
	}
}

func TestClusterTagForVolume(t *testing.T) {
	cases := []struct {
		tags []string
		want string
	}{
		{[]string{"lke613260"}, "lke613260"},
		{[]string{"lke-613260"}, "lke613260"}, // normalised
		{[]string{"some", "lke42"}, "lke42"},
		{[]string{"kubernetes"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := ClusterTagForVolume(c.tags); got != c.want {
			t.Errorf("ClusterTagForVolume(%v) = %q, want %q", c.tags, got, c.want)
		}
	}
}

func TestMergeClusterTag(t *testing.T) {
	cases := []struct {
		name    string
		tags    []string
		add     string
		want    []string
		changed bool
	}{
		{"adds missing", []string{"block-storage"}, "lke1", []string{"block-storage", "lke1"}, true},
		{"already present", []string{"block-storage", "lke1"}, "lke1", []string{"block-storage", "lke1"}, false},
		{"empty tag is noop", []string{"block-storage"}, "", []string{"block-storage"}, false},
		{"adds to empty", nil, "lke1", []string{"lke1"}, true},
	}
	for _, c := range cases {
		got, changed := MergeClusterTag(c.tags, c.add)
		if changed != c.changed || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: MergeClusterTag(%v,%q) = %v,%v want %v,%v", c.name, c.tags, c.add, got, changed, c.want, c.changed)
		}
	}
}

func TestInstanceIDFromProviderID(t *testing.T) {
	cases := map[string]string{
		"linode://12345": "12345",
		"linode://":      "",
		"aws://i-abc":    "",
		"":               "",
	}
	for in, want := range cases {
		if got := InstanceIDFromProviderID(in); got != want {
			t.Errorf("InstanceIDFromProviderID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVolumeAndInstanceAndUpdate(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v4/volumes/404":
			w.WriteHeader(404)
		case r.Method == http.MethodGet && r.URL.Path == "/v4/volumes/1":
			writeJSON(w, 200, map[string]any{"id": 1, "label": "pvc-x", "tags": []string{"block-storage"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v4/linode/instances/7":
			writeJSON(w, 200, map[string]any{"id": 7, "tags": []string{"kubernetes", "lke99"}})
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
	inst, status, err := c.Instance(ctx, "7")
	if err != nil || status != 200 || ClusterTagForVolume(MapTags(inst)) != "lke99" {
		t.Fatalf("Instance(7): tag=%q status %d err %v", ClusterTagForVolume(MapTags(inst)), status, err)
	}
	if status, err := c.UpdateVolume(ctx, "1", "new-label", []string{"block-storage", "lke99"}); err != nil || status != 200 {
		t.Fatalf("UpdateVolume = status %d err %v", status, err)
	}
	if gotMethod != http.MethodPut || gotPath != "/v4/volumes/1" {
		t.Errorf("last call = %s %s, want PUT /v4/volumes/1", gotMethod, gotPath)
	}
	if gotBody["label"] != "new-label" {
		t.Errorf("PUT body label = %v, want new-label", gotBody["label"])
	}
}
