package reconciler

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"
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

// ── C04: a watch that fails instantly used to hot-loop, silently ─────────────

// TestWatchBackoffRetreatsOnConsecutiveFailures.
//
// `_ = r.watch(ctx, fire)` discarded the error, and the reconnect pause was FLAT.
// A watch that fails immediately — RBAC denied on the collection, the CRD version
// gone, a field selector the apiserver rejects — returned at once, the catch-up
// fire() ran a FULL RELIST, and one second later it all happened again. Roughly
// 1Hz of list traffic against the apiserver for the life of the pod, with
// llz_reconcile_up pinned at 1 the whole time, because the reconcile passes were
// succeeding. It was the watch that was dead.
func TestWatchBackoffRetreatsOnConsecutiveFailures(t *testing.T) {
	prevBase, prevMax := watchReconnectBackoff, watchReconnectBackoffMax
	t.Cleanup(func() { watchReconnectBackoff, watchReconnectBackoffMax = prevBase, prevMax })
	watchReconnectBackoff, watchReconnectBackoffMax = time.Second, time.Minute

	// A clean close keeps the base pause — that is the case the constant was
	// written for and it must not get slower.
	if got := watchBackoffFor(0); got != time.Second {
		t.Errorf("a watch that ran and closed cleanly = %v, want the base %v", got, time.Second)
	}
	if got := watchBackoffFor(1); got != 2*time.Second {
		t.Errorf("after 1 failure = %v, want 2s", got)
	}
	if got := watchBackoffFor(4); got != 16*time.Second {
		t.Errorf("after 4 failures = %v, want 16s", got)
	}
	// Capped, so a permanently broken watch settles instead of retreating forever.
	if got := watchBackoffFor(30); got != time.Minute {
		t.Errorf("after 30 failures = %v, want the cap %v", got, time.Minute)
	}
}

// TestWatchLoopActuallyBacksOff is the half the function test above cannot cover.
// Asserting watchBackoffFor is exponential says nothing about whether the LOOP
// calls it: such a gate stays green with the call site reverted to the flat
// constant, which is precisely the defect. This records the durations the loop
// asks for.
func TestWatchLoopActuallyBacksOff(t *testing.T) {
	prevBase, prevMax, prevWait := watchReconnectBackoff, watchReconnectBackoffMax, watchBackoffWait
	t.Cleanup(func() {
		watchReconnectBackoff, watchReconnectBackoffMax, watchBackoffWait = prevBase, prevMax, prevWait
	})
	watchReconnectBackoff, watchReconnectBackoffMax = time.Second, time.Minute

	var mu sync.Mutex
	var waited []time.Duration
	watchBackoffWait = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		waited = append(waited, d)
		mu.Unlock()
		ch := make(chan time.Time, 1)
		ch <- time.Time{} // fire immediately; the test is about the VALUE asked for
		return ch
	}

	ctx, cancel := context.WithCancel(context.Background())
	var watches int32
	r := reconciler{
		name: "es-store-recovery",
		run:  func(context.Context) error { return nil },
		watch: func(context.Context, func()) error {
			if atomic.AddInt32(&watches, 1) >= 4 {
				cancel()
			}
			return errors.New("watch denied")
		},
	}
	runWatchReconcilerLoop(ctx, metrics.NewRegistry(), time.Now, r, nil)

	mu.Lock()
	got := append([]time.Duration(nil), waited...)
	mu.Unlock()
	if len(got) < 3 {
		t.Fatalf("expected at least 3 reconnect pauses, got %v", got)
	}
	for i := 1; i < 3; i++ {
		if got[i] <= got[i-1] {
			t.Errorf("pause %d (%v) did not grow past pause %d (%v) — the loop is still using a flat "+
				"interval, which is the ~1Hz apiserver relist loop this backoff exists to end",
				i, got[i], i-1, got[i-1])
		}
	}
}

// TestWatchFailurePublishesItsState. The error was discarded entirely: no log, no
// metric. An RBAC denial looked exactly like a healthy stream that happened to
// close, and llz_reconcile_up stayed 1 because the reconcile passes were fine.
func TestWatchFailurePublishesItsState(t *testing.T) {
	prevBase, prevMax := watchReconnectBackoff, watchReconnectBackoffMax
	t.Cleanup(func() { watchReconnectBackoff, watchReconnectBackoffMax = prevBase, prevMax })
	watchReconnectBackoff, watchReconnectBackoffMax = time.Millisecond, 2*time.Millisecond

	reg := metrics.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	var watches int32
	r := reconciler{
		name: "es-store-recovery",
		run:  func(context.Context) error { return nil },
		watch: func(ctx context.Context, _ func()) error {
			if atomic.AddInt32(&watches, 1) >= 3 {
				cancel()
			}
			return errors.New("clusterrole denies watch on clustersecretstores")
		},
	}
	runWatchReconcilerLoop(ctx, reg, time.Now, r, nil)

	var buf bytes.Buffer
	if _, err := reg.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "llz_watch_errors_total") {
		t.Error("a failing watch must publish llz_watch_errors_total — otherwise the only symptom is " +
			"apiserver load nobody attributes to this pod")
	}
	if !strings.Contains(out, `llz_watch_connected{reconciler="es-store-recovery"} 0`) {
		t.Errorf("a failing watch must publish llz_watch_connected 0; got:\n%s", out)
	}
}

// TestWatchConnectedIsPublishedBeforeTheFirstClose.
//
// llz_watch_connected was written only after r.watch RETURNED, so a healthy
// long-lived stream — the normal case — published no series at all: nothing
// existed from pod start until the first close. An alert on
// `llz_watch_connected == 0` cannot fire on a metric that is absent, which is
// exactly the state a wedged watch leaves behind, so the gauge could not do the
// one job it was added for.
func TestWatchConnectedIsPublishedBeforeTheFirstClose(t *testing.T) {
	prevWait := watchBackoffWait
	t.Cleanup(func() { watchBackoffWait = prevWait })
	watchBackoffWait = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}

	ctx, cancel := context.WithCancel(context.Background())
	reg := metrics.NewRegistry()
	open := make(chan struct{})
	r := reconciler{
		name: "es-store-recovery",
		run:  func(context.Context) error { return nil },
		watch: func(wctx context.Context, _ func()) error {
			close(open)
			<-wctx.Done() // a stream that stays up, which is what healthy looks like
			return wctx.Err()
		},
	}
	done := make(chan struct{})
	go func() { defer close(done); runWatchReconcilerLoop(ctx, reg, time.Now, r, nil) }()
	<-open

	var buf bytes.Buffer
	if _, err := reg.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done

	if !strings.Contains(buf.String(), `llz_watch_connected{reconciler="es-store-recovery"} 1`) {
		t.Errorf("a stream that is up published no llz_watch_connected series, so an alert on == 0 "+
			"has nothing to evaluate and a permanently dead watch pages nobody. Registry was:\n%s", buf.String())
	}
}

// TestAFailingWatchPinsTheGaugeAtZero — the other side. The gauge must not flap
// back to 1 at the backoff cadence just because the loop re-entered the watch;
// only a stream that closed CLEANLY may claim health.
func TestAFailingWatchPinsTheGaugeAtZero(t *testing.T) {
	prevWait := watchBackoffWait
	t.Cleanup(func() { watchBackoffWait = prevWait })
	watchBackoffWait = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}

	ctx, cancel := context.WithCancel(context.Background())
	reg := metrics.NewRegistry()
	var n int32
	r := reconciler{
		name: "es-store-recovery",
		run:  func(context.Context) error { return nil },
		watch: func(context.Context, func()) error {
			if atomic.AddInt32(&n, 1) >= 5 {
				cancel()
			}
			return errors.New("watch denied")
		},
	}
	runWatchReconcilerLoop(ctx, reg, time.Now, r, nil)

	var buf bytes.Buffer
	if _, err := reg.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `llz_watch_connected{reconciler="es-store-recovery"} 0`) {
		t.Errorf("a watch that never establishes must pin the gauge at 0:\n%s", buf.String())
	}
}
