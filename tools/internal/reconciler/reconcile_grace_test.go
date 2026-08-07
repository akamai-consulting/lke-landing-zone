package reconciler

// reconcile_grace_test.go — openbaoBootstrapGrace stayed with the RUNTIME when the
// lanes were extracted, and so did its test. The grace window is a scheduling
// policy (swallow failures until the first success) rather than lane logic, which
// is the same line the extraction drew everywhere else.

import (
	"context"
	"errors"
	"testing"
)

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
