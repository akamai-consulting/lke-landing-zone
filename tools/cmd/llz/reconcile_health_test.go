package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kube"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
)

// route is one path → (status, body) the health server replies with.
type route struct {
	status int
	body   any
}

func healthServer(t *testing.T, routes map[string]route) *kube.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if rt.status != 0 && rt.status != 200 {
			w.WriteHeader(rt.status)
			return
		}
		_ = json.NewEncoder(w).Encode(rt.body)
	}))
	t.Cleanup(srv.Close)
	return kube.NewClient(srv.URL, "tok", srv.Client())
}

func withReady(status string) map[string]any {
	return map[string]any{"status": map[string]any{"conditions": []any{
		map[string]any{"type": "Synced", "status": "True"},
		map[string]any{"type": "Ready", "status": status, "reason": "R", "message": "m"},
	}}}
}

func healthMetrics(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if _, err := reg.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return b.String()
}

func TestSampleESOStore(t *testing.T) {
	cases := []struct {
		name   string
		rt     route
		wantsV string
	}{
		{"ready", route{200, withReady("True")}, "llz_eso_store_ready{store=\"openbao\"} 1"},
		{"not ready", route{200, withReady("False")}, "llz_eso_store_ready{store=\"openbao\"} 0"},
		{"absent 404", route{404, nil}, "llz_eso_store_ready{store=\"openbao\"} 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := healthServer(t, map[string]route{"/apis/external-secrets.io/v1/clustersecretstores/openbao": c.rt})
			reg := metrics.NewRegistry()
			if err := sampleESOStore(context.Background(), client, reg); err != nil {
				t.Fatalf("sampleESOStore: %v", err)
			}
			if !strings.Contains(healthMetrics(t, reg), c.wantsV) {
				t.Errorf("want %q:\n%s", c.wantsV, healthMetrics(t, reg))
			}
		})
	}
}

func TestSampleESOStoreServerError(t *testing.T) {
	client := healthServer(t, map[string]route{"/apis/external-secrets.io/v1/clustersecretstores/openbao": {500, nil}})
	reg := metrics.NewRegistry()
	if err := sampleESOStore(context.Background(), client, reg); err == nil {
		t.Error("500 should surface an error")
	}
}

func TestSampleCertificates(t *testing.T) {
	cert := func(ns, name, ready string) map[string]any {
		c := withReady(ready)
		c["metadata"] = map[string]any{"namespace": ns, "name": name}
		return c
	}
	list := map[string]any{"items": []any{
		cert("cert-manager", "a", "True"),
		cert("istio-system", "b", "False"),
		cert("llz-openbao", "c", "True"),
	}}
	client := healthServer(t, map[string]route{certsPath: {200, list}})
	reg := metrics.NewRegistry()
	if err := sampleCertificates(context.Background(), client, reg); err != nil {
		t.Fatalf("sampleCertificates: %v", err)
	}
	out := healthMetrics(t, reg)
	if !strings.Contains(out, "llz_certificates_total 3") || !strings.Contains(out, "llz_certificates_not_ready 1") {
		t.Errorf("want total 3 / not_ready 1:\n%s", out)
	}
}

func TestSampleCertificatesCRDAbsent(t *testing.T) {
	client := healthServer(t, map[string]route{certsPath: {404, nil}})
	reg := metrics.NewRegistry()
	if err := sampleCertificates(context.Background(), client, reg); err != nil {
		t.Fatalf("404 should not error: %v", err)
	}
	if !strings.Contains(healthMetrics(t, reg), "llz_certificates_total 0") {
		t.Error("CRD-absent should report 0 certificates")
	}
}

func TestSampleOpenBaoPods(t *testing.T) {
	pod := func(ready string) map[string]any { return withReady(ready) }
	list := map[string]any{"items": []any{pod("True"), pod("True"), pod("False")}}
	// The path has a query string; the server matches on path only.
	client := healthServer(t, map[string]route{"/api/v1/namespaces/llz-openbao/pods": {200, list}})
	reg := metrics.NewRegistry()
	if err := sampleOpenBaoPods(context.Background(), client, reg); err != nil {
		t.Fatalf("sampleOpenBaoPods: %v", err)
	}
	out := healthMetrics(t, reg)
	if !strings.Contains(out, "llz_openbao_pods_total 3") || !strings.Contains(out, "llz_openbao_pods_ready 2") {
		t.Errorf("want total 3 / ready 2:\n%s", out)
	}
}

func TestSampleHealthAggregatesAndSurfacesError(t *testing.T) {
	// All three present and healthy → no error, all gauges set.
	routes := map[string]route{
		"/apis/external-secrets.io/v1/clustersecretstores/openbao": {200, withReady("True")},
		certsPath:                             {200, map[string]any{"items": []any{}}},
		"/api/v1/namespaces/llz-openbao/pods": {200, map[string]any{"items": []any{}}},
	}
	client := healthServer(t, routes)
	reg := metrics.NewRegistry()
	if err := sampleHealth(context.Background(), client, reg); err != nil {
		t.Fatalf("sampleHealth: %v", err)
	}
	if !strings.Contains(healthMetrics(t, reg), "llz_eso_store_ready") {
		t.Error("expected the ESO gauge to be published")
	}

	// A failing certs read propagates out of the aggregate.
	routes[certsPath] = route{500, nil}
	client = healthServer(t, routes)
	if err := sampleHealth(context.Background(), client, metrics.NewRegistry()); err == nil {
		t.Error("a failing sub-sample should surface from sampleHealth")
	}
}

// TestReadyConditionAgreesWithFindReady pins the behavior the two readers
// disagreed on. reconcile_es_store_recovery's objReadyStatus routes through
// health.FindReady (absent => "Unknown"); readyCondition hand-rolled the same
// walk and returned "". No verdict depended on it — ClassifyReady only branches
// on "True" — but the value is interpolated into the operator-facing detail,
// where "" renders as "(Ready= reason=)".
func TestReadyConditionAgreesWithFindReady(t *testing.T) {
	tests := []struct {
		name       string
		obj        map[string]any
		wantStatus string
		wantReason string
	}{
		{
			name: "Ready=True is read with its reason and message",
			obj: map[string]any{"status": map[string]any{"conditions": []any{
				map[string]any{"type": "Ready", "status": "True", "reason": "Valid", "message": "ok"},
			}}},
			wantStatus: "True", wantReason: "Valid",
		},
		{
			name: "Ready=False is read verbatim",
			obj: map[string]any{"status": map[string]any{"conditions": []any{
				map[string]any{"type": "Ready", "status": "False", "reason": "SecretSyncedError"},
			}}},
			wantStatus: "False", wantReason: "SecretSyncedError",
		},
		{
			// THE divergence: previously "" here, "Unknown" from the sibling reader.
			name:       "no conditions at all => Unknown, not empty",
			obj:        map[string]any{"status": map[string]any{}},
			wantStatus: "Unknown",
		},
		{
			name: "some other condition type only => Unknown",
			obj: map[string]any{"status": map[string]any{"conditions": []any{
				map[string]any{"type": "Synced", "status": "True"},
			}}},
			wantStatus: "Unknown",
		},
		{
			// A malformed entry must not blind the reader to a valid sibling.
			name: "non-object condition entries are skipped",
			obj: map[string]any{"status": map[string]any{"conditions": []any{
				"garbage",
				map[string]any{"type": "Ready", "status": "True"},
			}}},
			wantStatus: "True",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, r, _ := readyCondition(tt.obj)
			if s != tt.wantStatus {
				t.Errorf("status = %q, want %q", s, tt.wantStatus)
			}
			if tt.wantReason != "" && r != tt.wantReason {
				t.Errorf("reason = %q, want %q", r, tt.wantReason)
			}
			// Both readers must now answer identically for the same object.
			if got := objReadyStatus(tt.obj); got != s {
				t.Errorf("objReadyStatus = %q but readyCondition = %q — the two readers must agree", got, s)
			}
		})
	}
}
