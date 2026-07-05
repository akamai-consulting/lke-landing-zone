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

func TestSampleNodesSuccess(t *testing.T) {
	client := srvClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(nodeList(node("True"), node("True"), node("False")))
	})
	reg := metrics.NewRegistry()
	if err := sampleNodes(context.Background(), client, reg); err != nil {
		t.Fatalf("sampleNodes: %v", err)
	}
	out := renderReg(t, reg)
	for _, want := range []string{
		"llz_reconcile_nodes_ready 2",
		"llz_reconcile_nodes_total 3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
}

func TestSampleNodesAPIError(t *testing.T) {
	client := srvClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	reg := metrics.NewRegistry()
	if err := sampleNodes(context.Background(), client, reg); err == nil {
		t.Fatal("expected error on API failure")
	}
	// No node gauges published when the sample failed.
	if strings.Contains(renderReg(t, reg), "llz_reconcile_nodes_total") {
		t.Errorf("node gauges should not be set on failure")
	}
}

func TestSampleNodesKeepsLastGoodOnBlip(t *testing.T) {
	// A failing pass returns an error and leaves the prior node gauges untouched,
	// so a transient blip does not blank the last-known-good surface.
	var fail bool
	client := srvClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(nodeList(node("True")))
	})
	reg := metrics.NewRegistry()
	if err := sampleNodes(context.Background(), client, reg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	fail = true
	if err := sampleNodes(context.Background(), client, reg); err == nil {
		t.Fatal("second pass should error")
	}
	if !strings.Contains(renderReg(t, reg), "llz_reconcile_nodes_ready 1") {
		t.Errorf("last-known-good node gauge should survive a blip")
	}
}

func TestBuildReconcilers(t *testing.T) {
	reg := metrics.NewRegistry()
	client := srvClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(nodeList())
	})

	identity := func(r func(context.Context) error) func(context.Context) error { return r }

	// Defaults: only the observe reconciler.
	recs := buildReconcilers(reg, client, reconcileOpts{}, identity)
	if len(recs) != 1 || recs[0].name != "observe" {
		t.Fatalf("default set = %v, want [observe]", names(recs))
	}
	if recs[0].interval != 30*time.Second {
		t.Errorf("observe interval defaulted to %v, want 30s", recs[0].interval)
	}

	// All optional reconcilers enabled.
	recs = buildReconcilers(reg, client, reconcileOpts{
		reconcileArgoNudge: true, argoNudgeResync: 5 * time.Minute,
		reconcileCidrFW: true, cidrFWResync: 10 * time.Minute,
		reconcileVolLabels: true, volLabelsResync: time.Hour,
		reconcileSCDemote: true, scDemoteResync: 2 * time.Minute,
		reconcileLinodeCred: true, linodeCredInterval: time.Hour,
		reconcileHarbor: true, harborInterval: 5 * time.Minute,
	}, identity)
	want := []string{"observe", "argo-nudge", "cidr-firewall", "volume-labels", "sc-demote", "linode-creds", "harbor"}
	if got := names(recs); len(got) != len(want) {
		t.Fatalf("enabled set = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("reconciler %d = %q, want %q (%v)", i, got[i], want[i], got)
			}
		}
	}
	// The three watch reconcilers carry a watch closure; the timed ones do not.
	byName := map[string]reconciler{}
	for _, r := range recs {
		byName[r.name] = r
	}
	for _, n := range []string{"argo-nudge", "cidr-firewall", "volume-labels", "sc-demote"} {
		if byName[n].watch == nil {
			t.Errorf("%s should carry a watch closure", n)
		}
	}
	for _, n := range []string{"linode-creds", "harbor"} {
		if byName[n].watch != nil {
			t.Errorf("%s should be timed (no watch)", n)
		}
	}
}

func TestBuildReconcilersGatesDrivingOnly(t *testing.T) {
	reg := metrics.NewRegistry()
	client := srvClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(nodeList())
	})
	// A gate that always blocks: driving reconcilers must no-op, observe must not.
	var driveCalls, observeCalls int
	block := func(func(context.Context) error) func(context.Context) error {
		return func(context.Context) error { driveCalls++; return nil }
	}
	recs := buildReconcilers(reg, client, reconcileOpts{reconcileArgoNudge: true}, block)
	// recs[0] observe (ungated), recs[1] argo-nudge (gated to the blocking stub).
	_ = recs[0].run(context.Background())
	observeCalls++ // observe ran for real (hit the httptest server)
	_ = recs[1].run(context.Background())
	if driveCalls != 1 {
		t.Errorf("driving reconciler should route through the gate, got %d gate calls", driveCalls)
	}
	if observeCalls != 1 {
		t.Errorf("observe should not be gated")
	}
}

func names(recs []reconciler) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.name
	}
	return out
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
