package linode

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestObjRegion(t *testing.T) {
	for in, want := range map[string]string{
		"us-ord-1":  "us-ord",
		"us-ord-10": "us-ord",
		"us-sea-1":  "us-sea",
		"fr-par-1":  "fr-par",
		"us-ord":    "us-ord", // already a region — idempotent
	} {
		if got := objRegion(in); got != want {
			t.Errorf("objRegion(%q) = %q, want %q", in, got, want)
		}
	}
}

// The Linode OBJ API deprecated the `cluster` field in bucket_access in favour of
// `region` (the cluster id minus its -N ordinal). Assert the mint sends `region`,
// with the stripped value, and never the rejected `cluster` field.
func TestCreateObjectStorageKeyBuckets_SendsRegion(t *testing.T) {
	var body map[string]any
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v4/object-storage/keys" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, 200, map[string]any{"id": 1, "access_key": "AK", "secret_key": "SK"})
	})

	if _, err := c.CreateObjectStorageKeyBuckets(context.Background(), "loki-object-store", "us-ord-1",
		[]string{"platform-loki-chunks-e2e", "platform-loki-ruler-e2e", "platform-loki-admin-e2e"}, "read_write"); err != nil {
		t.Fatalf("CreateObjectStorageKeyBuckets: %v", err)
	}

	access, ok := body["bucket_access"].([]any)
	if !ok || len(access) != 3 {
		t.Fatalf("bucket_access = %v, want 3 entries", body["bucket_access"])
	}
	for i, a := range access {
		m := a.(map[string]any)
		if m["region"] != "us-ord" {
			t.Errorf("entry %d region = %v, want us-ord", i, m["region"])
		}
		if _, hasCluster := m["cluster"]; hasCluster {
			t.Errorf("entry %d still sends the deprecated cluster field: %v", i, m)
		}
		if m["permissions"] != "read_write" {
			t.Errorf("entry %d permissions = %v", i, m["permissions"])
		}
	}
}
