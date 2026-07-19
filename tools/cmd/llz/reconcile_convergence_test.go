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

// convApp builds an Application object with the sync/health fields the classifier
// reads; automated=true supplies a syncPolicy.automated so it isn't classified as
// "sync suspended".
func convApp(name, sync, health string, automated bool) map[string]any {
	m := map[string]any{
		"metadata": map[string]any{"name": name},
		"status": map[string]any{
			"sync":   map[string]any{"status": sync},
			"health": map[string]any{"status": health},
		},
	}
	if automated {
		m["spec"] = map[string]any{"syncPolicy": map[string]any{"automated": map[string]any{}}}
	}
	return m
}

// convergenceServer serves the given apps (or a status override) at the
// Applications collection path and returns a kube.Client pointed at it.
func convergenceServer(t *testing.T, apps []map[string]any, statusOverride int) *kube.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != argoAppsPath {
			http.NotFound(w, r)
			return
		}
		if statusOverride != 0 {
			w.WriteHeader(statusOverride)
			return
		}
		items := make([]any, len(apps))
		for i, a := range apps {
			items[i] = a
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	}))
	t.Cleanup(srv.Close)
	return kube.NewClient(srv.URL, "tok", srv.Client())
}

func convergenceState(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if _, err := reg.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return b.String()
}

func TestSampleConvergenceConverged(t *testing.T) {
	client := convergenceServer(t, []map[string]any{
		convApp("app-a", "Synced", "Healthy", true),
		convApp("app-b", "Synced", "Healthy", true),
	}, 0)
	reg := metrics.NewRegistry()
	if err := sampleConvergence(context.Background(), client, reg); err != nil {
		t.Fatalf("sampleConvergence: %v", err)
	}
	out := convergenceState(t, reg)
	for _, want := range []string{
		"llz_convergence_state 0",
		"llz_convergence_apps_failed 0",
		"llz_convergence_apps_pending 0",
		"llz_convergence_apps_observed 2", // state 0 is only meaningful with a corpus
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestSampleConvergenceInProgress(t *testing.T) {
	client := convergenceServer(t, []map[string]any{
		convApp("app-a", "Synced", "Healthy", true),
		convApp("app-b", "OutOfSync", "Progressing", true),
	}, 0)
	reg := metrics.NewRegistry()
	if err := sampleConvergence(context.Background(), client, reg); err != nil {
		t.Fatalf("sampleConvergence: %v", err)
	}
	out := convergenceState(t, reg)
	if !strings.Contains(out, "llz_convergence_state 2") {
		t.Errorf("want state 2 (in-progress):\n%s", out)
	}
	if !strings.Contains(out, "llz_convergence_apps_pending 1") {
		t.Errorf("want pending 1:\n%s", out)
	}
}

func TestSampleConvergenceHardFailed(t *testing.T) {
	client := convergenceServer(t, []map[string]any{
		convApp("app-a", "Synced", "Healthy", true),
		convApp("app-b", "OutOfSync", "Degraded", true),
		convApp("app-c", "OutOfSync", "Progressing", true),
	}, 0)
	reg := metrics.NewRegistry()
	// A hard-failed cluster is a valid observation, not a sampler error.
	if err := sampleConvergence(context.Background(), client, reg); err != nil {
		t.Fatalf("sampleConvergence should not error on an unhealthy cluster: %v", err)
	}
	out := convergenceState(t, reg)
	if !strings.Contains(out, "llz_convergence_state 1") {
		t.Errorf("want state 1 (hard-failed dominates):\n%s", out)
	}
	if !strings.Contains(out, "llz_convergence_apps_failed 1") {
		t.Errorf("want failed 1:\n%s", out)
	}
}

func TestSampleConvergenceCRDAbsentIsInProgress(t *testing.T) {
	client := convergenceServer(t, nil, http.StatusNotFound)
	reg := metrics.NewRegistry()
	// 404 on the collection = Applications CRD not installed yet (pre-bootstrap).
	if err := sampleConvergence(context.Background(), client, reg); err != nil {
		t.Fatalf("404 should be in-progress, not an error: %v", err)
	}
	if !strings.Contains(convergenceState(t, reg), "llz_convergence_state 2") {
		t.Error("CRD-absent should report state 2 (in-progress)")
	}
}

// convergenceRawServer serves an arbitrary JSON body at the Applications path,
// for the malformed-collection cases convergenceServer cannot express.
func convergenceRawServer(t *testing.T, body string) *kube.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != argoAppsPath {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return kube.NewClient(srv.URL, "tok", srv.Client())
}

// An unread corpus must not report the same green as a healthy one. Each case
// here used to render as llz_convergence_state=0 — "all Argo Applications
// converged" — on the strength of having classified nothing at all.
func TestSampleConvergenceEmptyCorpusIsNotConverged(t *testing.T) {
	cases := []struct {
		name         string
		client       func(t *testing.T) *kube.Client
		wantObserved string
		wantPending  string
	}{
		{
			name:         "empty list (CRD registered, bootstrap has not created apps)",
			client:       func(t *testing.T) *kube.Client { return convergenceServer(t, nil, 0) },
			wantObserved: "llz_convergence_apps_observed 0",
			wantPending:  "llz_convergence_apps_pending 1",
		},
		{
			name:         "items key absent",
			client:       func(t *testing.T) *kube.Client { return convergenceRawServer(t, `{"kind":"ApplicationList"}`) },
			wantObserved: "llz_convergence_apps_observed 0",
			wantPending:  "llz_convergence_apps_pending 1",
		},
		{
			name:         "items is not an array",
			client:       func(t *testing.T) *kube.Client { return convergenceRawServer(t, `{"items":{"oops":1}}`) },
			wantObserved: "llz_convergence_apps_observed 0",
			wantPending:  "llz_convergence_apps_pending 1",
		},
		{
			name:         "every item unparseable",
			client:       func(t *testing.T) *kube.Client { return convergenceRawServer(t, `{"items":["nope","also-nope"]}`) },
			wantObserved: "llz_convergence_apps_observed 0",
			wantPending:  "llz_convergence_apps_pending 1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reg := metrics.NewRegistry()
			if err := sampleConvergence(context.Background(), c.client(t), reg); err != nil {
				t.Fatalf("sampleConvergence: %v", err)
			}
			out := convergenceState(t, reg)
			if !strings.Contains(out, "llz_convergence_state 2") {
				t.Errorf("nothing was classified — want state 2 (in-progress), got:\n%s", out)
			}
			for _, want := range []string{c.wantObserved, c.wantPending} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q:\n%s", want, out)
				}
			}
		})
	}
}

// A partial read is partial: the apps that DID parse are still classified, and
// the ones that did not are unknown rather than healthy.
func TestSampleConvergenceUnparsedAppsAreUnknownNotHealthy(t *testing.T) {
	app, err := json.Marshal(convApp("app-a", "Synced", "Healthy", true))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	client := convergenceRawServer(t, `{"items":[`+string(app)+`,"unparseable"]}`)
	reg := metrics.NewRegistry()
	if err := sampleConvergence(context.Background(), client, reg); err != nil {
		t.Fatalf("sampleConvergence: %v", err)
	}
	out := convergenceState(t, reg)
	for _, want := range []string{
		"llz_convergence_state 2",         // not 0 — one app's health is unknown
		"llz_convergence_apps_observed 1", // the healthy one still counted
		"llz_convergence_apps_pending 1",  // the unparseable one
		"llz_convergence_apps_failed 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// The gate reads the same report: an empty corpus must not exit 0.
func TestHealthInClusterGateRejectsEmptyCorpus(t *testing.T) {
	r, crdPresent, err := convergenceReport(context.Background(), convergenceServer(t, nil, 0))
	if err != nil {
		t.Fatalf("convergenceReport: %v", err)
	}
	if got := convergenceExit(r, crdPresent, true); got != 2 {
		t.Errorf("health-incluster on zero Applications: exit %d, want 2 (in-progress)", got)
	}
}

// The CRD-absent path must publish its counts, not leave the previous sample's
// on the endpoint — the registry never expires a gauge.
func TestSampleConvergenceCRDAbsentClearsCounts(t *testing.T) {
	reg := metrics.NewRegistry()
	healthy := convergenceServer(t, []map[string]any{
		convApp("app-a", "Synced", "Healthy", true),
		convApp("app-b", "OutOfSync", "Degraded", true),
	}, 0)
	if err := sampleConvergence(context.Background(), healthy, reg); err != nil {
		t.Fatalf("sampleConvergence: %v", err)
	}
	if !strings.Contains(convergenceState(t, reg), "llz_convergence_apps_observed 2") {
		t.Fatal("setup: expected 2 observed apps")
	}
	// Now the CRD goes away (404) on the same registry.
	if err := sampleConvergence(context.Background(), convergenceServer(t, nil, http.StatusNotFound), reg); err != nil {
		t.Fatalf("sampleConvergence 404: %v", err)
	}
	out := convergenceState(t, reg)
	for _, want := range []string{
		"llz_convergence_apps_observed 0",
		"llz_convergence_apps_failed 0",
		"llz_convergence_apps_pending 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stale count survived the CRD-absent sample, missing %q:\n%s", want, out)
		}
	}
}

func TestSampleConvergenceAPIErrorSurfaces(t *testing.T) {
	client := convergenceServer(t, nil, http.StatusInternalServerError)
	reg := metrics.NewRegistry()
	if err := sampleConvergence(context.Background(), client, reg); err == nil {
		t.Error("a 500 should surface an error (observe records up=0)")
	}
}
