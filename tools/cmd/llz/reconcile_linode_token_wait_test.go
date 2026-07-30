package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// withFastTokenPoll shrinks the poll so the tests do not sleep.
func withFastTokenPoll(t *testing.T) {
	t.Helper()
	prev := linodeTokenPollInterval
	linodeTokenPollInterval = time.Millisecond
	t.Cleanup(func() { linodeTokenPollInterval = prev })
}

// TestWithLinodeTokenWait_KicksWhenTokenArrives reproduces the bootstrap ordering
// that left two consecutive clusters with pvc-<uuid> Volume labels:
//
//	all watch events fire  →  token is still absent  →  lane no-ops
//	token seeded           →  no further watch events exist  →  nothing re-runs
//
// The wrapper must notice the arrival and kick the lane, because the lane's only
// other trigger is a 1-hour resync floor.
func TestWithLinodeTokenWait_KicksWhenTokenArrives(t *testing.T) {
	withFastTokenPoll(t)
	t.Setenv("LINODE_TOKEN", "") // absent, as on a cold bootstrap

	var kicks int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The inner watch delivers its events immediately and then goes quiet forever —
	// exactly like a PV watch after apl-core has finished creating every PVC.
	inner := func(ctx context.Context, onEvent func()) error {
		onEvent()
		<-ctx.Done()
		return nil
	}
	go func() { _ = withLinodeTokenWait(inner)(ctx, func() { atomic.AddInt32(&kicks, 1) }) }()

	// The one inner event lands while the token is missing.
	waitForToken(t, func() bool { return atomic.LoadInt32(&kicks) >= 1 }, "inner watch event")
	before := atomic.LoadInt32(&kicks)

	// The credential finally shows up — the moment nothing used to notice.
	t.Setenv("LINODE_TOKEN", "seeded-by-mint-bootstrap-pat")

	waitForToken(t, func() bool { return atomic.LoadInt32(&kicks) > before },
		"a kick AFTER the token arrived (without it the Volumes keep pvc-<uuid> labels until the 1h resync)")
}

// TestWithLinodeTokenWait_NoOpWhenTokenAlreadyPresent: on every pass after the
// first bootstrap the token exists, and the wrapper must then cost nothing — no
// spurious kicks, so no extra Linode API traffic on a steady-state cluster.
func TestWithLinodeTokenWait_NoOpWhenTokenAlreadyPresent(t *testing.T) {
	withFastTokenPoll(t)
	t.Setenv("LINODE_TOKEN", "already-there")

	var kicks int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inner := func(ctx context.Context, _ func()) error { <-ctx.Done(); return nil }
	go func() { _ = withLinodeTokenWait(inner)(ctx, func() { atomic.AddInt32(&kicks, 1) }) }()

	time.Sleep(20 * time.Millisecond) // several poll intervals
	if got := atomic.LoadInt32(&kicks); got != 0 {
		t.Fatalf("wrapper kicked %d time(s) with the token already present — it must be inert in steady state", got)
	}
}

// TestWaitForLinodeTokenThenKick_HonoursContext: the waiter must not outlive the
// reconciler, or a rolling restart leaks a goroutine per lane per pod.
func TestWaitForLinodeTokenThenKick_HonoursContext(t *testing.T) {
	withFastTokenPoll(t)
	t.Setenv("LINODE_TOKEN", "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		waitForLinodeTokenThenKick(ctx, func() { t.Error("must not kick without a token") })
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not exit on ctx cancellation — this leaks a goroutine per lane")
	}
}

// waitForToken polls cond until true or the deadline, failing with what was expected.
func waitForToken(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
