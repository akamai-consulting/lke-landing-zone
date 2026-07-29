package kube

// Edge coverage for the API client's success/failure boundary, the nil-client
// default, the in-cluster timeout and truncate's cutoff — all of which mutation
// testing showed were unasserted.
//
// Every verb here decides success with `status < 200 || status >= 300`, but the
// suite only exercised 200/201/404/409/403/422 — nothing on either edge.
// Relaxing `>= 300` to `> 300` makes an HTTP 300 read as success in all four
// verbs, and tightening `< 200` to `<= 200` makes a plain 200 an ERROR in
// CreateJSON (where the existing test only ever sees 201 Created and 409
// Conflict); both survived the whole suite.
//
// So each verb is driven at 200 / 299 / 300 with a body that DECODES CLEANLY —
// otherwise a verb that wrongly admitted 300 would still fail on the parse and
// the test would prove nothing about the status check.
//
// 199 (the lower edge) is deliberately absent: Go's net/http server treats a 1xx
// WriteHeader as an informational header and keeps the response open, so a 199
// cannot be delivered as a final status through the httptest seam.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type kubeVerb struct {
	name string
	body string
	call func(context.Context, *Client) error
}

var kubeVerbs = []kubeVerb{
	{
		name: "GetJSON",
		body: `{"kind":"ConfigMap"}`,
		call: func(ctx context.Context, c *Client) error {
			_, _, err := c.GetJSON(ctx, "/api/v1/namespaces/kube-system/configmaps/x")
			return err
		},
	},
	{
		name: "CreateJSON",
		body: `{}`,
		call: func(ctx context.Context, c *Client) error {
			_, err := c.CreateJSON(ctx, "/api/v1/namespaces/kube-system/configmaps", map[string]any{"kind": "ConfigMap"})
			return err
		},
	},
	{
		name: "MergePatch",
		body: `{}`,
		call: func(ctx context.Context, c *Client) error {
			return c.MergePatch(ctx, "/api/v1/namespaces/kube-system/configmaps/x", map[string]any{"data": map[string]string{"k": "v"}})
		},
	},
	{
		name: "Watch",
		body: `{"type":"ADDED","object":{}}`,
		call: func(ctx context.Context, c *Client) error {
			return c.Watch(ctx, "/api/v1/pods", "", func(WatchEvent) error { return nil })
		},
	},
}

func TestVerbSuccessBoundaryIs2xx(t *testing.T) {
	edges := []struct {
		status  int
		wantErr bool
	}{
		// 200 matters on its own: CreateJSON's only existing success assertion is
		// at 201, so a lower edge of `<= 200` turns an OK into a failed create.
		{http.StatusOK, false},
		{299, false},
		{300, true}, // the first NON-success status: a redirect is not a result
	}
	for _, v := range kubeVerbs {
		for _, e := range edges {
			t.Run(fmt.Sprintf("%s/%d", v.name, e.status), func(t *testing.T) {
				c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(e.status)
					_, _ = w.Write([]byte(v.body))
				})
				err := v.call(context.Background(), c)
				if e.wantErr && err == nil {
					t.Errorf("HTTP %d was accepted as success — the 2xx boundary is wrong", e.status)
				}
				if !e.wantErr && err != nil {
					t.Errorf("HTTP %d should succeed, got %v", e.status, err)
				}
			})
		}
	}
}

// TestCreateJSONSurfacesTransportlessFailure pins CreateJSON's `if err != nil`
// early return. Inverted, it returns (status, nil) on every SUCCESSFUL request —
// short-circuiting both the 409 branch and the non-2xx check below it, so a 500
// from the apiserver reads as "created". The 2xx table above cannot see this:
// there the two paths agree.
func TestCreateJSONSurfacesTransportlessFailure(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "etcdserver: request timed out", http.StatusInternalServerError)
	})
	status, err := c.CreateJSON(context.Background(), "/api/v1/namespaces/x/configmaps", map[string]any{})
	if err == nil {
		t.Fatalf("CreateJSON = (%d, nil) on a 500 — a failed create must not look like a success", status)
	}
}

// TestNewClientDefaultsHTTPClient pins the `if httpClient == nil` default.
// Inverted, a nil argument stays nil (every later call nil-derefs on c.http.Do)
// and a caller-supplied client is thrown away for the default — which for the
// in-cluster path would discard the ServiceAccount CA transport.
func TestNewClientDefaultsHTTPClient(t *testing.T) {
	c := NewClient("https://kubernetes.default.svc/", "tok", nil)
	if c.http == nil {
		t.Fatal("NewClient(nil) left a nil *http.Client — every request nil-derefs")
	}
	if c.http.Timeout != 30*time.Second {
		t.Errorf("default client Timeout = %v, want 30s", c.http.Timeout)
	}
	custom := &http.Client{Timeout: 7 * time.Second}
	if got := NewClient("https://kubernetes.default.svc", "tok", custom); got.http != custom {
		t.Error("a caller-supplied *http.Client must be used as-is, not replaced by the default")
	}
}

// TestNewInClusterHTTPTimeout pins the in-pod client's 30s Timeout. `30 *
// time.Second` mutates to `30 / time.Second`, which is 0 — i.e. NO timeout — and
// nothing noticed. That matters here specifically: do() is used by CronJobs and
// the reconciler against the in-cluster apiserver, and Watch is the one caller
// that deliberately drops this Timeout (see its comment), so a silently-zero
// value would leave a wedged apiserver call hanging forever with no deadline.
func TestNewInClusterHTTPTimeout(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/token", []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/ca.crt", selfSignedCAPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SA_TOKEN_FILE", dir+"/token")
	t.Setenv("SA_CA_FILE", dir+"/ca.crt")

	c, err := NewInCluster()
	if err != nil {
		t.Fatalf("NewInCluster: %v", err)
	}
	if c.http.Timeout != 30*time.Second {
		t.Errorf("in-cluster client Timeout = %v, want 30s (0 means no deadline at all)", c.http.Timeout)
	}
}

// TestTruncateBoundary pins truncate's cutoff at exactly max. `len(s) > max`
// relaxed to `>= max` clips a payload that is exactly at the limit and appends an
// ellipsis claiming there was more — the one length where the two disagree.
func TestTruncateBoundary(t *testing.T) {
	const max = 300
	exact := strings.Repeat("x", max)
	if got := truncate([]byte(exact)); got != exact {
		t.Errorf("a %d-byte payload is at the limit, not over it: got %d bytes (%q…)", max, len(got), got[:10])
	}
	if got, want := truncate([]byte(exact+"y")), exact+"…"; got != want {
		t.Errorf("a %d-byte payload must be truncated with an ellipsis, got %q", max+1, got)
	}
	if got := truncate([]byte("  boom  ")); got != "boom" {
		t.Errorf("truncate(%q) = %q, want the trimmed payload", "  boom  ", got)
	}
}
