package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kube"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
)

func nodeList(nodes ...map[string]any) map[string]any {
	items := make([]any, len(nodes))
	for i, n := range nodes {
		items[i] = n
	}
	return map[string]any{"items": items}
}

func node(ready string) map[string]any {
	return map[string]any{
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": "MemoryPressure", "status": "False"},
				map[string]any{"type": "Ready", "status": ready},
			},
		},
	}
}

func TestTallyNodeReadiness(t *testing.T) {
	cases := []struct {
		name             string
		list             map[string]any
		wantR, wantTotal int
	}{
		{"empty", nodeList(), 0, 0},
		{"all ready", nodeList(node("True"), node("True")), 2, 2},
		{"mixed", nodeList(node("True"), node("False"), node("Unknown")), 1, 3},
		{"no items key", map[string]any{}, 0, 0},
		{"malformed item counts to total only", nodeList(nil), 0, 1},
		{"node without status", nodeList(map[string]any{}), 0, 1},
		{"node without ready condition", nodeList(map[string]any{
			"status": map[string]any{"conditions": []any{
				map[string]any{"type": "DiskPressure", "status": "False"},
			}},
		}), 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, total := tallyNodeReadiness(tc.list)
			if r != tc.wantR || total != tc.wantTotal {
				t.Fatalf("tallyNodeReadiness = (%d,%d), want (%d,%d)", r, total, tc.wantR, tc.wantTotal)
			}
		})
	}
}

// srvClient spins up an httptest server serving the given /api/v1/nodes handler
// and returns a real kube.Client pointed at it.
func srvClient(t *testing.T, h http.HandlerFunc) *kube.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/nodes" {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return kube.NewClient(srv.URL, "test-token", srv.Client())
}

func renderReg(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if _, err := reg.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return b.String()
}

func TestSampleSuccess(t *testing.T) {
	client := srvClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(nodeList(node("True"), node("True"), node("False")))
	})
	reg := metrics.NewRegistry()
	now := time.Unix(1751644800, 0)
	sample(context.Background(), client, reg, now)

	out := renderReg(t, reg)
	for _, want := range []string{
		"llz_reconcile_up 1",
		"llz_reconcile_nodes_ready 2",
		"llz_reconcile_nodes_total 3",
		"llz_reconcile_last_sample_timestamp_seconds 1.7516448e+09",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
}

func TestSampleAPIErrorSetsDown(t *testing.T) {
	client := srvClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	reg := metrics.NewRegistry()
	sample(context.Background(), client, reg, time.Unix(1751644800, 0))

	out := renderReg(t, reg)
	if !strings.Contains(out, "llz_reconcile_up 0") {
		t.Errorf("expected up=0 on API error:\n%s", out)
	}
	// Timestamp still advances so a staleness alert can tell "wedged" from "failing".
	if !strings.Contains(out, "llz_reconcile_last_sample_timestamp_seconds ") {
		t.Errorf("timestamp should advance even on failure:\n%s", out)
	}
	// No node gauges published when the sample failed.
	if strings.Contains(out, "llz_reconcile_nodes_total") {
		t.Errorf("node gauges should not be set on failure:\n%s", out)
	}
}

func TestSampleKeepsLastGoodOnBlip(t *testing.T) {
	// First a good sample, then a failing one: the level gauges from the good
	// sample must survive (we don't erase last-known-good on a transient blip).
	var fail bool
	client := srvClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(nodeList(node("True")))
	})
	reg := metrics.NewRegistry()
	sample(context.Background(), client, reg, time.Unix(1, 0))
	fail = true
	sample(context.Background(), client, reg, time.Unix(2, 0))

	out := renderReg(t, reg)
	if !strings.Contains(out, "llz_reconcile_up 0") {
		t.Errorf("up should be 0 after the failing sample:\n%s", out)
	}
	if !strings.Contains(out, "llz_reconcile_nodes_ready 1") {
		t.Errorf("last-known-good node gauge should survive a blip:\n%s", out)
	}
}

func TestRunReconcileShutsDownOnCancel(t *testing.T) {
	client := srvClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(nodeList(node("True")))
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		// :0 → an OS-assigned free port, so the test never collides.
		done <- runReconcile(ctx, client, reconcileOpts{metricsAddr: "127.0.0.1:0", sampleInterval: time.Hour})
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runReconcile returned %v, want clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runReconcile did not return after context cancel")
	}
}
