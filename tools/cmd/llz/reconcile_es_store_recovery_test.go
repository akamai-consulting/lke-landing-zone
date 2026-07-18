package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kube"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
)

// esRecoveryServer serves the store (mutable readiness), the ES/PushSecret
// lists, and records every PATCH path.
type esRecoveryServer struct {
	mu      sync.Mutex
	ready   string // "True" / "False"
	storeNF bool   // serve 404 for the store
	esReady string // Ready status every listed ExternalSecret reports
	patched []string
}

func (s *esRecoveryServer) start(t *testing.T) *kube.Client {
	t.Helper()
	obj := func(ns, name, ready string) map[string]any {
		return map[string]any{
			"metadata": map[string]any{"namespace": ns, "name": name},
			"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": ready}}},
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == esStorePath:
			if s.storeNF {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "code": 404})
				return
			}
			_ = json.NewEncoder(w).Encode(obj("", defaultSecretStore, s.ready))
		case r.Method == http.MethodGet && r.URL.Path == esListPath:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{
				obj("harbor", "harbor-registry-s3", s.esReady),
				obj("monitoring", "loki-object-store", s.esReady),
			}})
		case r.Method == http.MethodGet && r.URL.Path == pushListPath:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{
				obj("harbor", "harbor-admin-push", s.esReady),
			}})
		case r.Method == http.MethodPatch:
			s.patched = append(s.patched, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return kube.NewClient(srv.URL, "tok", srv.Client())
}

func (s *esRecoveryServer) patchedPaths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.patched...)
}

func (s *esRecoveryServer) set(ready string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = ready
}

func TestESStoreRecoveryBumpsOnTransitionOnly(t *testing.T) {
	fixedNow(t, 4242)
	srv := &esRecoveryServer{ready: "False", esReady: "False"}
	client := srv.start(t)
	reg := metrics.NewRegistry()
	lane := &esStoreRecovery{}

	// Not ready → observed, no bump.
	if err := lane.reconcile(context.Background(), client, reg); err != nil {
		t.Fatalf("not-ready pass: %v", err)
	}
	if got := srv.patchedPaths(); len(got) != 0 {
		t.Fatalf("no bump while not Ready, got %v", got)
	}

	// Transition to Ready → one fan-out over both kinds (2 ES + 1 PushSecret).
	srv.set("True")
	if err := lane.reconcile(context.Background(), client, reg); err != nil {
		t.Fatalf("transition pass: %v", err)
	}
	got := srv.patchedPaths()
	if len(got) != 3 {
		t.Fatalf("transition must bump 3 objects, got %v", got)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"/apis/external-secrets.io/v1/namespaces/harbor/externalsecrets/harbor-registry-s3",
		"/apis/external-secrets.io/v1/namespaces/monitoring/externalsecrets/loki-object-store",
		"/apis/external-secrets.io/v1alpha1/namespaces/harbor/pushsecrets/harbor-admin-push",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing patch %q in %v", want, got)
		}
	}

	// Still Ready → steady state, no further bumps.
	if err := lane.reconcile(context.Background(), client, reg); err != nil {
		t.Fatalf("steady pass: %v", err)
	}
	if got := srv.patchedPaths(); len(got) != 3 {
		t.Fatalf("steady state must not re-bump, got %v", got)
	}
}

func TestESStoreRecoveryRestartAmnesty(t *testing.T) {
	fixedNow(t, 4242)
	// A fresh lane (restart) that first observes Ready with stale ExternalSecrets
	// bumps once; with everything already Ready it stays quiet.
	srv := &esRecoveryServer{ready: "True", esReady: "False"}
	client := srv.start(t)
	reg := metrics.NewRegistry()
	if err := (&esStoreRecovery{}).reconcile(context.Background(), client, reg); err != nil {
		t.Fatalf("amnesty pass: %v", err)
	}
	if got := srv.patchedPaths(); len(got) != 3 {
		t.Fatalf("restart with stale ES must bump, got %v", got)
	}

	quiet := &esRecoveryServer{ready: "True", esReady: "True"}
	qc := quiet.start(t)
	if err := (&esStoreRecovery{}).reconcile(context.Background(), qc, metrics.NewRegistry()); err != nil {
		t.Fatalf("all-ready pass: %v", err)
	}
	if got := quiet.patchedPaths(); len(got) != 0 {
		t.Fatalf("restart with everything Ready must not bump, got %v", got)
	}
}

func TestESStoreRecoveryStoreAbsentIsObservedNotError(t *testing.T) {
	srv := &esRecoveryServer{storeNF: true}
	client := srv.start(t)
	lane := &esStoreRecovery{}
	if err := lane.reconcile(context.Background(), client, metrics.NewRegistry()); err != nil {
		t.Fatalf("404 store must not error (pre-bootstrap), got %v", err)
	}
	if lane.lastReady != "false" {
		t.Fatalf("absent store must record lastReady=false, got %q", lane.lastReady)
	}
}

func TestInclusterLinodeToken(t *testing.T) {
	dir := t.TempDir()
	prev := linodeTokenFile
	linodeTokenFile = filepath.Join(dir, "token")
	t.Cleanup(func() { linodeTokenFile = prev })

	// Neither env nor file → empty.
	t.Setenv("LINODE_TOKEN", "")
	if got := inclusterLinodeToken(); got != "" {
		t.Fatalf("no source: got %q", got)
	}
	// File appears (the optional volume materializing) → picked up lazily.
	if err := os.WriteFile(linodeTokenFile, []byte("file-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := inclusterLinodeToken(); got != "file-tok" {
		t.Fatalf("file source: got %q", got)
	}
	// Env wins (CronJob/CI compatibility).
	t.Setenv("LINODE_TOKEN", "env-tok")
	if got := inclusterLinodeToken(); got != "env-tok" {
		t.Fatalf("env precedence: got %q", got)
	}
}

// TestOpenbaoBootstrapGrace: unreachable OpenBao is swallowed until the first
// success (wave-0 bootstrap window), then real errors surface (day-2 outage).
func TestOpenbaoBootstrapGrace(t *testing.T) {
	var out error
	wrapped := openbaoBootstrapGrace(func(context.Context) error { return out })

	// Bootstrap window: OpenBao unreachable → swallowed (no error, no alert churn).
	out = errors.New("connection refused")
	if err := wrapped(context.Background()); err != nil {
		t.Fatalf("pre-first-success unreachable must be swallowed, got %v", err)
	}
	// OpenBao comes up.
	out = nil
	if err := wrapped(context.Background()); err != nil {
		t.Fatalf("reachable pass: %v", err)
	}
	// Day-2 outage AFTER it was once up → the error must surface (alert should fire).
	out = errors.New("connection refused")
	if err := wrapped(context.Background()); err == nil {
		t.Fatal("a failure after the first success is a real outage and must surface")
	}
}
