package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kube"
)

func TestFailedAppName(t *testing.T) {
	failed := func(name, phase string) map[string]any {
		return map[string]any{
			"metadata": map[string]any{"name": name},
			"status":   map[string]any{"operationState": map[string]any{"phase": phase}},
		}
	}
	cases := []struct {
		name     string
		item     any
		wantName string
		wantOK   bool
	}{
		{"failed app", failed("a", "Failed"), "a", true},
		{"succeeded app", failed("b", "Succeeded"), "b", false},
		{"running app", failed("c", "Running"), "c", false},
		{"no operationState", map[string]any{"metadata": map[string]any{"name": "d"}}, "d", false},
		{"no name", map[string]any{"status": map[string]any{"operationState": map[string]any{"phase": "Failed"}}}, "", false},
		{"not a map", "nope", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, ok := failedAppName(tc.item)
			if n != tc.wantName || ok != tc.wantOK {
				t.Fatalf("failedAppName = (%q,%v), want (%q,%v)", n, ok, tc.wantName, tc.wantOK)
			}
		})
	}
}

// argoTestServer serves the Applications list and records every PATCH by app name.
func argoTestServer(t *testing.T, apps []map[string]any) (*kube.Client, *[]string, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var patched []string
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == argoAppsPath:
			items := make([]any, len(apps))
			for i, a := range apps {
				items[i] = a
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, argoAppsPath+"/"):
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			patched = append(patched, strings.TrimPrefix(r.URL.Path, argoAppsPath+"/"))
			bodies = append(bodies, string(body))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return kube.NewClient(srv.URL, "tok", srv.Client()), &patched, &bodies
}

func app(name, phase string) map[string]any {
	m := map[string]any{"metadata": map[string]any{"name": name}}
	if phase != "" {
		m["status"] = map[string]any{"operationState": map[string]any{"phase": phase}}
	}
	return m
}

func TestReconcileArgoNudgePatchesOnlyFailed(t *testing.T) {
	client, patched, bodies := argoTestServer(t, []map[string]any{
		app("wedged-1", "Failed"),
		app("healthy", "Succeeded"),
		app("syncing", "Running"),
		app("wedged-2", "Failed"),
		app("fresh", ""), // no operationState
	})
	if err := reconcileArgoNudge(context.Background(), client); err != nil {
		t.Fatalf("reconcileArgoNudge: %v", err)
	}
	got := append([]string(nil), *patched...)
	if len(got) != 2 || !contains(got, "wedged-1") || !contains(got, "wedged-2") {
		t.Fatalf("patched = %v, want exactly [wedged-1 wedged-2]", got)
	}
	// The patch re-triggers: hard refresh + a fresh sync operation.
	b := (*bodies)[0]
	if !strings.Contains(b, `"argocd.argoproj.io/refresh":"hard"`) || !strings.Contains(b, `"sync":{}`) {
		t.Errorf("patch body missing refresh+sync: %s", b)
	}
}

func TestReconcileArgoNudgeNoFailedIsNoOp(t *testing.T) {
	client, patched, _ := argoTestServer(t, []map[string]any{
		app("healthy", "Succeeded"),
		app("syncing", "Running"),
	})
	if err := reconcileArgoNudge(context.Background(), client); err != nil {
		t.Fatalf("reconcileArgoNudge: %v", err)
	}
	if len(*patched) != 0 {
		t.Fatalf("expected no patches, got %v", *patched)
	}
}

func TestReconcileArgoNudgeListErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	client := kube.NewClient(srv.URL, "tok", srv.Client())
	if err := reconcileArgoNudge(context.Background(), client); err == nil {
		t.Fatal("expected error when listing Applications fails")
	}
}

// A transient git-fetch ComparisonError (no Failed sync op) is now nudged too.
func TestTransientComparisonErrorApp(t *testing.T) {
	transient := map[string]any{
		"metadata": map[string]any{"name": "platform-bootstrap"},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "ComparisonError", "message": "failed to list refs: repository not found"},
		}},
	}
	if name, ok := transientComparisonErrorApp(transient); !ok || name != "platform-bootstrap" {
		t.Fatalf("transient ComparisonError should nudge, got name=%q ok=%v", name, ok)
	}
	// A real manifest error must NOT be nudged.
	real := map[string]any{
		"metadata": map[string]any{"name": "app"},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "ComparisonError", "message": "is missing required field kind"},
		}},
	}
	if _, ok := transientComparisonErrorApp(real); ok {
		t.Error("a real manifest ComparisonError must not be nudged")
	}
	// No conditions → not nudged.
	if _, ok := transientComparisonErrorApp(map[string]any{"metadata": map[string]any{"name": "x"}}); ok {
		t.Error("an app with no ComparisonError must not be nudged")
	}
}

// TestGitAuthErrorIsNotNudged — a credential the remote rejected is the one
// git-fetch failure a hard refresh provably cannot recover. Two of
// transientFetchError's patterns ("failed to list refs", "could not read") match
// an auth refusal, so without the explicit guard the nudge lane re-asks the same
// rejected question every poll for the whole convergence budget.
func TestGitAuthErrorIsNotNudged(t *testing.T) {
	for _, msg := range []string{
		"failed to list refs: authentication required: Unauthorized",
		"could not read Username for 'https://github.com': terminal prompts disabled",
	} {
		if transientFetchError(msg) {
			t.Errorf("auth refusal must not be nudged as transient: %q", msg)
		}
	}
	// The flakes it exists for must still nudge.
	for _, msg := range []string{
		"failed to list refs: dial tcp 140.82.113.4:443: i/o timeout",
		"failed to list refs: repository not found",
	} {
		if !transientFetchError(msg) {
			t.Errorf("genuine transient must still nudge: %q", msg)
		}
	}
}
