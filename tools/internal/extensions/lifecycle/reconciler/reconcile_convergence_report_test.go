package reconciler

// TestConvergenceReport stayed with its subject.
//
// It travelled to internal/converge inside ci_health_incluster_test.go, but
// ConvergenceReport is the RECONCILER's classifier in reconcile_convergence.go —
// converge only consumes it, through a seam that returns a verdict.

import (
	"context"
	"net/http"
	"testing"
)

// ConvergenceReport is the kubectl-free classifier the health-incluster verb shares
// with the reconciler gauge. Reuses convergenceServer/convApp from
// reconcile_convergence_test.go.
func TestConvergenceReport(t *testing.T) {
	// Converged.
	r, crd, err := ConvergenceReport(context.Background(),
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
	r, _, _ = ConvergenceReport(context.Background(),
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
	if _, crd, err := ConvergenceReport(context.Background(), convergenceServer(t, nil, http.StatusNotFound)); err != nil || crd {
		t.Errorf("404 should be crdPresent=false, no error; got crd=%v err=%v", crd, err)
	}

	// API error (500) → surfaced as an error (apiserver-unreachable class).
	if _, _, err := ConvergenceReport(context.Background(), convergenceServer(t, nil, http.StatusInternalServerError)); err == nil {
		t.Error("a 500 should surface an error")
	}
}
