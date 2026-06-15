package linode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// clientFor stands up an httptest server running h and returns a Client whose
// base URL points at it — the seam that lets the request builders run without
// touching the real Linode API.
func clientFor(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient("tok", 5*time.Second)
	c.base = srv.URL
	return c
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// dataPage serves a single-page Linode collection response for listAllPages.
func dataPage(items ...map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"data": items, "pages": 1})
	}
}

func TestClusterK8sVersion(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("missing/incorrect auth header: %q", got)
		}
		if r.URL.Path != "/v4beta/lke/clusters/42" {
			t.Errorf("path = %q, want /v4beta/lke/clusters/42", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{"k8s_version": "v1.31.9+lke7"})
	})
	got, err := c.ClusterK8sVersion(context.Background(), 42)
	if err != nil || got != "v1.31.9+lke7" {
		t.Fatalf("ClusterK8sVersion = (%q, %v), want (v1.31.9+lke7, nil)", got, err)
	}
}

func TestClusterK8sVersionError(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})
	if _, err := c.ClusterK8sVersion(context.Background(), 1); err == nil {
		t.Error("ClusterK8sVersion on 403 = nil error, want error")
	}
}

func TestDeleteKubeconfig(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v4/lke/clusters/7/kubeconfig" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := c.DeleteKubeconfig(context.Background(), 7); err != nil {
		t.Errorf("DeleteKubeconfig = %v, want nil", err)
	}
}

func TestListProfileTokensPaginates(t *testing.T) {
	// page 1 advertises 2 pages; the loop must fetch both and concatenate.
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			writeJSON(w, http.StatusOK, map[string]any{"data": []map[string]any{{"id": 1}}, "pages": 2})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"data": []map[string]any{{"id": 2}}, "pages": 2})
		}
	})
	toks, err := c.ListProfileTokens(context.Background())
	if err != nil || len(toks) != 2 {
		t.Fatalf("ListProfileTokens = (%d items, %v), want (2, nil)", len(toks), err)
	}
}

func TestListAllPagesError(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad scope", http.StatusUnauthorized)
	})
	if _, err := c.ListProfileTokens(context.Background()); err == nil {
		t.Error("listAllPages on 401 = nil error, want error")
	}
}

func TestCreateProfileToken(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": 5, "token": "secret-once"})
	})
	out, err := c.CreateProfileToken(context.Background(), "ci", "*", "2030-01-01T00:00:00")
	if err != nil || out["token"] != "secret-once" {
		t.Fatalf("CreateProfileToken = (%v, %v), want token secret-once", out, err)
	}
}

func TestCreateProfileTokenError(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid scopes", http.StatusBadRequest)
	})
	if _, err := c.CreateProfileToken(context.Background(), "ci", "bad", "x"); err == nil {
		t.Error("CreateProfileToken on 400 = nil error, want error")
	}
}

func TestDeleteProfileToken(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v4/profile/tokens/9" {
			t.Errorf("path = %q, want /v4/profile/tokens/9", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.DeleteProfileToken(context.Background(), 9); err != nil {
		t.Errorf("DeleteProfileToken = %v, want nil", err)
	}
}

func TestObjectStorageKeyCRUD(t *testing.T) {
	ctx := context.Background()
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			writeJSON(w, http.StatusOK, map[string]any{"id": 3, "access_key": "AK", "secret_key": "SK"})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default: // GET list
			writeJSON(w, http.StatusOK, map[string]any{"data": []map[string]any{{"id": 3}}, "pages": 1})
		}
	})
	if out, err := c.CreateObjectStorageKey(ctx, "lbl", "us-ord-1", "bkt", "read_write"); err != nil || out["secret_key"] != "SK" {
		t.Fatalf("CreateObjectStorageKey = (%v, %v)", out, err)
	}
	if keys, err := c.ListObjectStorageKeys(ctx); err != nil || len(keys) != 1 {
		t.Fatalf("ListObjectStorageKeys = (%d, %v)", len(keys), err)
	}
	if err := c.DeleteObjectStorageKey(ctx, 3); err != nil {
		t.Errorf("DeleteObjectStorageKey = %v, want nil", err)
	}
}

func TestObjectStorageClustersAndBucket(t *testing.T) {
	ctx := context.Background()
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeJSON(w, http.StatusOK, map[string]any{"label": "b", "cluster": "us-ord-1"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data": []map[string]any{{"id": "us-ord-1", "domain": "us-ord-1.linodeobjects.com"}}, "pages": 1,
		})
	})
	if cls, err := c.ListObjectStorageClusters(ctx); err != nil || len(cls) != 1 {
		t.Fatalf("ListObjectStorageClusters = (%d, %v)", len(cls), err)
	}
	if out, err := c.CreateObjectStorageBucket(ctx, "us-ord-1", "b"); err != nil || out["label"] != "b" {
		t.Fatalf("CreateObjectStorageBucket = (%v, %v)", out, err)
	}
}

func TestLiveClusterIDsAndLabel(t *testing.T) {
	ctx := context.Background()
	c := clientFor(t, dataPage(
		map[string]any{"id": 7, "label": "prod"},
		map[string]any{"id": 8, "label": "staging"},
	))
	live, err := c.LiveClusterIDs(ctx)
	if err != nil || !live["7"] || !live["8"] || len(live) != 2 {
		t.Fatalf("LiveClusterIDs = (%v, %v)", live, err)
	}
	ids, err := c.ClustersWithLabel(ctx, "staging")
	if err != nil || len(ids) != 1 || ids[0] != 8 {
		t.Fatalf("ClustersWithLabel(staging) = (%v, %v), want [8]", ids, err)
	}
}

func TestNodeBalancerBackendCount(t *testing.T) {
	c := clientFor(t, dataPage(
		map[string]any{"nodes_status": map[string]any{"up": 2, "down": 1}},
		map[string]any{"nodes_status": map[string]any{"up": 3, "down": 0}},
	))
	n, err := c.NodeBalancerBackendCount(context.Background(), 5)
	if err != nil || n != 6 {
		t.Fatalf("NodeBalancerBackendCount = (%d, %v), want (6, nil)", n, err)
	}
}

func TestListHelpers(t *testing.T) {
	ctx := context.Background()
	c := clientFor(t, dataPage(map[string]any{"id": 1}))
	for name, fn := range map[string]func() ([]map[string]any, error){
		"ListNodeBalancers": func() ([]map[string]any, error) { return c.ListNodeBalancers(ctx) },
		"ListVPCs":          func() ([]map[string]any, error) { return c.ListVPCs(ctx) },
		"ListVPCSubnets":    func() ([]map[string]any, error) { return c.ListVPCSubnets(ctx, 1) },
		"ListVolumes":       func() ([]map[string]any, error) { return c.ListVolumes(ctx) },
		"ListFirewalls":     func() ([]map[string]any, error) { return c.ListFirewalls(ctx) },
	} {
		got, err := fn()
		if err != nil || len(got) != 1 {
			t.Errorf("%s = (%d, %v), want (1, nil)", name, len(got), err)
		}
	}
}

func TestDeleteResourcePath(t *testing.T) {
	ctx := context.Background()
	// 200 and 404 are both success; 500 is an error.
	for _, code := range []int{http.StatusOK, http.StatusNotFound} {
		c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
		if err := c.DeleteResourcePath(ctx, "/v4/nodebalancers/1"); err != nil {
			t.Errorf("DeleteResourcePath on %d = %v, want nil", code, err)
		}
	}
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) })
	if err := c.DeleteResourcePath(ctx, "/v4/nodebalancers/1"); err == nil {
		t.Error("DeleteResourcePath on 500 = nil error, want error")
	}
}

func TestUpdateControlPlaneACL(t *testing.T) {
	// Delegates to PutControlPlaneACL: the correct control_plane_acl (underscore)
	// endpoint with an `acl`-wrapped body. The old control_plane/acl path 404s.
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v4beta/lke/clusters/2/control_plane_acl" {
			t.Errorf("path = %q, want .../control_plane_acl", r.URL.Path)
		}
		var body struct {
			ACL struct {
				Enabled   bool `json:"enabled"`
				Addresses struct {
					IPv4 []string `json:"ipv4"`
				} `json:"addresses"`
			} `json:"acl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if !body.ACL.Enabled || len(body.ACL.Addresses.IPv4) != 1 || body.ACL.Addresses.IPv4[0] != "1.2.3.0/24" {
			t.Errorf("body acl = %+v, want enabled + ipv4=[1.2.3.0/24]", body.ACL)
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := c.UpdateControlPlaneACL(context.Background(), 2, []string{"1.2.3.0/24"}, nil); err != nil {
		t.Errorf("UpdateControlPlaneACL = %v, want nil", err)
	}
}
