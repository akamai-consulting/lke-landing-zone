package linode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFindIDByLabel(t *testing.T) {
	items := []map[string]any{
		{"id": json.Number("1"), "label": "a"},
		{"id": json.Number("2"), "label": "primary-vpc"},
		{"id": json.Number("3"), "label": "b"},
	}
	if id, ok := FindIDByLabel(items, "primary-vpc"); !ok || id != 2 {
		t.Errorf("FindIDByLabel = (%d,%v), want (2,true)", id, ok)
	}
	if _, ok := FindIDByLabel(items, "absent"); ok {
		t.Error("absent label should return ok=false")
	}
	if _, ok := FindIDByLabel(nil, "x"); ok {
		t.Error("empty collection should return ok=false")
	}
}

func TestListNodePoolsPaginates(t *testing.T) {
	// Two-page response: the target pool only appears on page 2, which the old
	// single-page `page_size=500` query class of bug would have missed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			w.Write([]byte(`{"data":[{"id":1,"label":"other"}],"pages":2}`))
			return
		}
		w.Write([]byte(`{"data":[{"id":2,"label":"observability-pool"}],"pages":2}`))
	}))
	defer srv.Close()

	c := NewClient("tok", 5*time.Second)
	c.base = srv.URL
	pools, err := c.ListNodePools(context.Background(), 99)
	if err != nil {
		t.Fatalf("ListNodePools: %v", err)
	}
	if id, ok := FindIDByLabel(pools, "observability-pool"); !ok || id != 2 {
		t.Errorf("paginated lookup = (%d,%v), want (2,true) — page 2 not fetched?", id, ok)
	}
}

func TestSumInstanceVCPUs(t *testing.T) {
	instances := []map[string]any{
		{"specs": map[string]any{"vcpus": json.Number("2")}},
		{"specs": map[string]any{"vcpus": json.Number("4")}},
		{"label": "no-specs"}, // missing specs => contributes 0
	}
	if got := SumInstanceVCPUs(instances); got != 6 {
		t.Errorf("SumInstanceVCPUs = %d, want 6", got)
	}
	if got := SumInstanceVCPUs(nil); got != 0 {
		t.Errorf("SumInstanceVCPUs(nil) = %d, want 0", got)
	}
}

func TestListInstancesAndTypeVCPUs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v4/linode/instances":
			w.Write([]byte(`{"data":[{"id":1,"specs":{"vcpus":4}},{"id":2,"specs":{"vcpus":2}}],"pages":1}`))
		case r.URL.Path == "/v4/linode/types/g6-standard-4":
			w.Write([]byte(`{"vcpus":4}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := NewClient("tok", 5*time.Second)
	c.base = srv.URL

	instances, err := c.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if got := SumInstanceVCPUs(instances); got != 6 {
		t.Errorf("SumInstanceVCPUs(live) = %d, want 6", got)
	}
	v, err := c.LinodeTypeVCPUs(context.Background(), "g6-standard-4")
	if err != nil || v != 4 {
		t.Errorf("LinodeTypeVCPUs = (%d,%v), want 4", v, err)
	}
	// Unknown type -> 0, no error (caller treats pool draw as 0).
	if v, err := c.LinodeTypeVCPUs(context.Background(), "nonesuch"); err != nil || v != 0 {
		t.Errorf("unknown type = (%d,%v), want (0,nil)", v, err)
	}
}

func TestGetKubeconfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"kubeconfig":"YXBpVmVyc2lvbjogdjE="}`))
	}))
	defer srv.Close()
	c := NewClient("tok", 5*time.Second)
	c.base = srv.URL
	kc, err := c.GetKubeconfig(context.Background(), 1)
	if err != nil || kc != "YXBpVmVyc2lvbjogdjE=" {
		t.Errorf("GetKubeconfig = (%q,%v), want the base64 blob", kc, err)
	}

	// Non-2xx -> ("", nil) so the caller writes a stub.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	c.base = bad.URL
	if kc, err := c.GetKubeconfig(context.Background(), 1); err != nil || kc != "" {
		t.Errorf("503 GetKubeconfig = (%q,%v), want empty+nil", kc, err)
	}
}
