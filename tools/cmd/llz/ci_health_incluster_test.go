package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
)

// convergenceReport is the kubectl-free classifier the health-incluster verb shares
// with the reconciler gauge. Reuses convergenceServer/convApp from
// reconcile_convergence_test.go.
func TestConvergenceReport(t *testing.T) {
	// Converged.
	r, crd, err := convergenceReport(context.Background(),
		convergenceServer(t, []map[string]any{convApp("a", "Synced", "Healthy", true)}, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !crd {
		t.Fatal("crdPresent should be true when the collection returns 200")
	}
	if r.ExitCode() != 0 {
		t.Errorf("converged: exit %d, want 0", r.ExitCode())
	}

	// Hard-failed (a Degraded app dominates).
	r, _, _ = convergenceReport(context.Background(),
		convergenceServer(t, []map[string]any{
			convApp("a", "Synced", "Healthy", true),
			convApp("b", "OutOfSync", "Degraded", true),
		}, 0))
	if r.ExitCode() != 1 {
		t.Errorf("degraded: exit %d, want 1", r.ExitCode())
	}
	if len(r.Failed) != 1 {
		t.Errorf("want 1 failed, got %d", len(r.Failed))
	}

	// CRD absent (404) → crdPresent false, no error (pre-bootstrap).
	if _, crd, err := convergenceReport(context.Background(), convergenceServer(t, nil, http.StatusNotFound)); err != nil || crd {
		t.Errorf("404 should be crdPresent=false, no error; got crd=%v err=%v", crd, err)
	}

	// API error (500) → surfaced as an error (apiserver-unreachable class).
	if _, _, err := convergenceReport(context.Background(), convergenceServer(t, nil, http.StatusInternalServerError)); err == nil {
		t.Error("a 500 should surface an error")
	}
}

func TestConvergenceExit(t *testing.T) {
	failed := health.Report{Failed: []string{"x hard-failed"}}
	pending := health.Report{Pending: []string{"y in-progress"}}
	ok := health.Report{}
	cases := []struct {
		name        string
		r           health.Report
		crd, failOn bool
		want        int
	}{
		{"converged-gate", ok, true, true, 0},
		{"failed-gate", failed, true, true, 1},
		{"pending-gate", pending, true, true, 2},
		{"pre-bootstrap-gate", ok, false, true, 2}, // CRD absent = in-progress
		{"failed-report-only", failed, true, false, 0},
		{"pending-report-only", pending, true, false, 0},
		{"pre-bootstrap-report-only", ok, false, false, 0},
	}
	for _, c := range cases {
		if got := convergenceExit(c.r, c.crd, c.failOn); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
