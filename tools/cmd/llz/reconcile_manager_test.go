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
	go func() { runManager(ctx, reg, fixedClock(0), []reconciler{rec}, nil); close(done) }()

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

func TestRunManagerKickTriggersExtraPass(t *testing.T) {
	reg := metrics.NewRegistry()
	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// interval 0 → without a kick this reconciler runs exactly once then holds.
	// This is the sc-demote cold-start shape: a gated pass that would otherwise
	// wait a resync floor. A kick (leader acquisition) must re-run it immediately.
	rec := reconciler{name: "kicked", interval: 0, run: func(context.Context) error {
		calls.Add(1)
		return nil
	}}
	k := &kicker{}
	done := make(chan struct{})
	go func() { runManager(ctx, reg, fixedClock(0), []reconciler{rec}, k); close(done) }()

	waitCount := func(want int64, what string) {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for calls.Load() < want {
			select {
			case <-deadline:
				t.Fatalf("%s: got %d passes, want %d", what, calls.Load(), want)
			default:
				time.Sleep(2 * time.Millisecond)
			}
		}
	}
	waitCount(1, "initial pass")
	k.Kick()
	waitCount(2, "pass after first kick")
	k.Kick()
	waitCount(3, "pass after second kick")
	cancel()
	<-done
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
	go func() { runManager(ctx, reg, fixedClock(0), []reconciler{rec}, nil); close(done) }()

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
	go func() { runManager(ctx, reg, fixedClock(0), recs, nil); close(done) }()

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

func TestWatchReconcilerEventTriggersRun(t *testing.T) {
	defer swapBackoff(5 * time.Millisecond)()
	reg := metrics.NewRegistry()
	var runs atomic.Int64
	events := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())

	rec := reconciler{
		name:     "w",
		interval: 0, // no resync floor — isolate the event path
		run:      func(context.Context) error { runs.Add(1); return nil },
		watch: func(ctx context.Context, onEvent func()) error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-events:
					onEvent()
				}
			}
		},
	}
	done := make(chan struct{})
	go func() { runManager(ctx, reg, fixedClock(0), []reconciler{rec}, nil); close(done) }()

	waitFor(t, &runs, 1) // initial pass
	events <- struct{}{}
	events <- struct{}{}
	waitFor(t, &runs, 2) // at least one event-triggered pass (bursts may coalesce)

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("watch reconciler did not stop on cancel")
	}
}

func TestWatchReconcilerResyncFloor(t *testing.T) {
	defer swapBackoff(5 * time.Millisecond)()
	reg := metrics.NewRegistry()
	var runs atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())

	// A watch that never fires an event and never returns until ctx: only the
	// resync floor should drive reconciles.
	rec := reconciler{
		name:     "w",
		interval: 15 * time.Millisecond,
		run:      func(context.Context) error { runs.Add(1); return nil },
		watch:    func(ctx context.Context, _ func()) error { <-ctx.Done(); return ctx.Err() },
	}
	done := make(chan struct{})
	go func() { runManager(ctx, reg, fixedClock(0), []reconciler{rec}, nil); close(done) }()

	waitFor(t, &runs, 3) // 1 initial + resync ticks
	cancel()
	<-done
}

func TestWatchReconcilerReconnectsAndCatchesUp(t *testing.T) {
	defer swapBackoff(5 * time.Millisecond)()
	reg := metrics.NewRegistry()
	var runs, watchCalls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())

	// A watch that returns immediately (stream "closed") each time; the loop must
	// re-establish it and fire a catch-up reconcile between attempts.
	rec := reconciler{
		name:     "w",
		interval: 0,
		run:      func(context.Context) error { runs.Add(1); return nil },
		watch: func(ctx context.Context, _ func()) error {
			watchCalls.Add(1)
			return nil // simulate a clean stream close
		},
	}
	done := make(chan struct{})
	go func() { runManager(ctx, reg, fixedClock(0), []reconciler{rec}, nil); close(done) }()

	waitFor(t, &watchCalls, 2) // proves re-establishment after a close
	waitFor(t, &runs, 2)       // initial + at least one reconnect catch-up
	cancel()
	<-done
}

// swapBackoff sets watchReconnectBackoff for a test and returns a restorer.
func swapBackoff(d time.Duration) func() {
	old := watchReconnectBackoff
	watchReconnectBackoff = d
	return func() { watchReconnectBackoff = old }
}

// waitFor blocks until counter reaches want (or fails after 2s).
func waitFor(t *testing.T, counter *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for counter.Load() < want {
		select {
		case <-deadline:
			t.Fatalf("counter reached %d, want >= %d", counter.Load(), want)
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
}
