package reconcilelanes

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"
)

// esRecoveryServer serves the store (mutable readiness), the ES/PushSecret
// lists, and records every PATCH path.
type esRecoveryServer struct {
	mu      sync.Mutex
	ready   string // "True" / "False"
	storeNF bool   // serve 404 for the store
	esReady string // Ready status every listed ExternalSecret reports
	patched []string
	// patchFails makes the next patchFails PATCHes 409, so a PARTIAL fan-out can
	// be driven — which is the state that used to consume the recovery transition
	// and then never retry it.
	patchFails int
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
			_ = json.NewEncoder(w).Encode(obj("", DefaultSecretStore, s.ready))
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
			if s.patchFails > 0 {
				s.patchFails--
				w.WriteHeader(http.StatusConflict)
				return
			}
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
	lane := &ESStoreRecovery{}

	// Not ready → observed, no bump.
	if err := lane.Reconcile(context.Background(), client, reg); err != nil {
		t.Fatalf("not-ready pass: %v", err)
	}
	if got := srv.patchedPaths(); len(got) != 0 {
		t.Fatalf("no bump while not Ready, got %v", got)
	}

	// Transition to Ready → one fan-out over both kinds (2 ES + 1 PushSecret).
	srv.set("True")
	if err := lane.Reconcile(context.Background(), client, reg); err != nil {
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
	if err := lane.Reconcile(context.Background(), client, reg); err != nil {
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
	if err := (&ESStoreRecovery{}).Reconcile(context.Background(), client, reg); err != nil {
		t.Fatalf("amnesty pass: %v", err)
	}
	if got := srv.patchedPaths(); len(got) != 3 {
		t.Fatalf("restart with stale ES must bump, got %v", got)
	}

	quiet := &esRecoveryServer{ready: "True", esReady: "True"}
	qc := quiet.start(t)
	if err := (&ESStoreRecovery{}).Reconcile(context.Background(), qc, metrics.NewRegistry()); err != nil {
		t.Fatalf("all-ready pass: %v", err)
	}
	if got := quiet.patchedPaths(); len(got) != 0 {
		t.Fatalf("restart with everything Ready must not bump, got %v", got)
	}
}

func TestESStoreRecoveryStoreAbsentIsObservedNotError(t *testing.T) {
	srv := &esRecoveryServer{storeNF: true}
	client := srv.start(t)
	lane := &ESStoreRecovery{}
	if err := lane.Reconcile(context.Background(), client, metrics.NewRegistry()); err != nil {
		t.Fatalf("404 store must not error (pre-bootstrap), got %v", err)
	}
	if lane.lastReady != "false" {
		t.Fatalf("absent store must record lastReady=false, got %q", lane.lastReady)
	}
}

// TestESStoreRecoveryRetriesAfterAPartialFanOut.
//
// s.lastReady was assigned BEFORE forceSyncESKinds, so a failed or PARTIAL fan-out
// — one MergePatch 409, one list 403, a CRD version drift on pushsecrets — still
// recorded the store as Ready. The next poll then matched neither
// `ready && lastReady == "false"` nor the first-observation amnesty branch, so it
// never bumped again: the notReady->Ready transition this lane exists to catch was
// spent, and every ExternalSecret the fan-out missed was left to ESO's own
// ~16-minute backoff with nothing retrying it.
func TestESStoreRecoveryRetriesAfterAPartialFanOut(t *testing.T) {
	fixedNow(t, 4242)
	srv := &esRecoveryServer{ready: "False", esReady: "False"}
	client := srv.start(t)
	reg := metrics.NewRegistry()
	lane := &ESStoreRecovery{}

	// Poll 1: store not ready — observe, no bump.
	if err := lane.Reconcile(context.Background(), client, reg); err != nil {
		t.Fatal(err)
	}
	// Poll 2: store goes Ready, but the fan-out fails on the first object.
	srv.set("True")
	srv.mu.Lock()
	srv.patchFails = 1
	srv.mu.Unlock()
	if err := lane.Reconcile(context.Background(), client, reg); err == nil {
		t.Fatal("a fan-out that could not patch every object must report it")
	}
	partial := len(srv.patchedPaths())

	// Poll 3: nothing about the world changed — the store is still Ready. The
	// transition must NOT have been consumed.
	if err := lane.Reconcile(context.Background(), client, reg); err != nil {
		t.Fatalf("the retry must succeed: %v", err)
	}
	if len(srv.patchedPaths()) <= partial {
		t.Errorf("no objects were patched on the retry (%d then %d) — the recovery transition was "+
			"consumed by a fan-out that did not finish, so the ExternalSecrets it missed now wait on "+
			"ESO's ~16-minute backoff with nothing retrying them",
			partial, len(srv.patchedPaths()))
	}
}

// TestESStoreRecoveryCountsOnlyCompletedFanOuts. The nudge counter was incremented
// before the error was checked, so a fan-out that patched nothing was
// indistinguishable from one that patched everything — on the one counter an
// operator would consult to ask which it was.
func TestESStoreRecoveryCountsOnlyCompletedFanOuts(t *testing.T) {
	fixedNow(t, 4242)
	srv := &esRecoveryServer{ready: "False", esReady: "False"}
	client := srv.start(t)
	reg := metrics.NewRegistry()
	lane := &ESStoreRecovery{}

	_ = lane.Reconcile(context.Background(), client, reg)
	srv.set("True")
	srv.mu.Lock()
	srv.patchFails = 99 // every patch fails
	srv.mu.Unlock()
	_ = lane.Reconcile(context.Background(), client, reg)

	var buf bytes.Buffer
	if _, err := reg.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "llz_es_recovery_nudges_total 1") {
		t.Error("a fan-out in which every patch failed must not count as a nudge")
	}
}
