package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
)

// fixedClock returns a now() that advances by 1s each call, so duration is
// deterministic and last-success timestamps are reproducible.
func fixedClock(start int64) func() time.Time {
	var n atomic.Int64
	n.Store(start)
	return func() time.Time { return time.Unix(n.Add(1), 0) }
}

func TestReconcileOnceSuccessMetrics(t *testing.T) {
	reg := metrics.NewRegistry()
	r := reconciler{name: "obs", run: func(context.Context) error { return nil }}
	reconcileOnce(context.Background(), reg, fixedClock(100), r)

	out := renderReg(t, reg)
	for _, want := range []string{
		`llz_reconcile_runs_total{reconciler="obs"} 1`,
		`llz_reconcile_errors_total{reconciler="obs"} 0`,
		`llz_reconcile_up{reconciler="obs"} 1`,
		`llz_reconcile_last_success_timestamp_seconds{reconciler="obs"} 102`, // 2nd clock read (end)
		"# TYPE llz_reconcile_runs_total counter",
		"# TYPE llz_reconcile_up gauge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestReconcileOnceErrorMetrics(t *testing.T) {
	reg := metrics.NewRegistry()
	r := reconciler{name: "obs", run: func(context.Context) error { return errors.New("boom") }}
	reconcileOnce(context.Background(), reg, fixedClock(0), r)

	out := renderReg(t, reg)
	for _, want := range []string{
		`llz_reconcile_runs_total{reconciler="obs"} 1`,
		`llz_reconcile_errors_total{reconciler="obs"} 1`,
		`llz_reconcile_up{reconciler="obs"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	// No success timestamp on a failed pass.
	if strings.Contains(out, "llz_reconcile_last_success_timestamp_seconds") {
		t.Errorf("failed pass must not set a success timestamp:\n%s", out)
	}
}

func TestRunManagerRunsThenStopsOnCancel(t *testing.T) {
	reg := metrics.NewRegistry()
	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())

	// interval 0 → runs exactly once then holds until ctx is cancelled.
	rec := reconciler{name: "once", interval: 0, run: func(context.Context) error {
		calls.Add(1)
		return nil
	}}
	done := make(chan struct{})
	go func() { runManager(ctx, reg, fixedClock(0), []reconciler{rec}); close(done) }()

	// Give the single pass a moment, then cancel.
	deadline := time.After(2 * time.Second)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("reconciler never ran")
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runManager did not return after cancel")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("interval-0 reconciler ran %d times, want exactly 1", got)
	}
}

func TestRunManagerTicks(t *testing.T) {
	reg := metrics.NewRegistry()
	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())

	rec := reconciler{name: "tick", interval: 15 * time.Millisecond, run: func(context.Context) error {
		calls.Add(1)
		return nil
	}}
	done := make(chan struct{})
	go func() { runManager(ctx, reg, fixedClock(0), []reconciler{rec}); close(done) }()

	// Wait for at least a few ticks (1 initial + ticker), then stop.
	deadline := time.After(2 * time.Second)
	for calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d ticks in 2s; ticker not firing", calls.Load())
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	cancel()
	<-done
}

func TestRunManagerConcurrentReconcilers(t *testing.T) {
	reg := metrics.NewRegistry()
	var a, b atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	recs := []reconciler{
		{name: "a", interval: 0, run: func(context.Context) error { a.Add(1); return nil }},
		{name: "b", interval: 0, run: func(context.Context) error { b.Add(1); return nil }},
	}
	done := make(chan struct{})
	go func() { runManager(ctx, reg, fixedClock(0), recs); close(done) }()

	deadline := time.After(2 * time.Second)
	for a.Load() == 0 || b.Load() == 0 {
		select {
		case <-deadline:
			t.Fatalf("both reconcilers should have run once: a=%d b=%d", a.Load(), b.Load())
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	cancel()
	<-done
	if !strings.Contains(renderReg(t, reg), `llz_reconcile_up{reconciler="b"} 1`) {
		t.Error("reconciler b metrics missing")
	}
}
