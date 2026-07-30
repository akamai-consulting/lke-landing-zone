package main

// Gap-closing tests for the destroy-path plumbing in ci.go: where sweepUntilEmpty
// backs off between attempts, and how waitVolumesDetached polls and reports.
//
// Both are log surfaces as much as control flow — the destroy job's log is all an
// operator has when a teardown leaves orphans behind — so the wording and the
// attempt numbers are pinned alongside the loop behavior.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The retry notice belongs BETWEEN attempts and nowhere else. Printing it after
// the final attempt promises a retry that never comes (and pays a pointless
// sleep before the terminal error); printing it only after the final attempt
// leaves the intermediate waits unexplained.
func TestSweepUntilEmptyRetryNoticeOnlyBetweenAttempts(t *testing.T) {
	p := &sweepProbe{remaining: []int{2}} // never converges: every attempt is spent
	out := captureStdout(t, func() {
		if err := sweepUntilEmpty(context.Background(), confirmOpts, nil, testSweepOpts(3, true), p.sweep, p.count); err == nil {
			t.Error("sweepUntilEmpty = nil, want the orphans-remain error")
		}
	})
	if got := strings.Count(out, "retrying the Widget sweep"); got != 2 {
		t.Errorf("retry notices = %d, want 2 (1→2 and 2→3, never after the last attempt):\n%s", got, out)
	}
	if p.sweeps != 3 {
		t.Errorf("sweeps = %d, want 3", p.sweeps)
	}
}

// A retry that does not wait is three calls into the same instant. The Linode
// API needs a beat to catch up with a delete that has already been accepted, so
// the configured delay must actually be spent.
func TestSweepUntilEmptyWaitsTheRetryDelay(t *testing.T) {
	o := testSweepOpts(2, true)
	o.retryDelay = 1 // seconds
	p := &sweepProbe{remaining: []int{1}}

	start := time.Now()
	captureStdout(t, func() {
		_ = sweepUntilEmpty(context.Background(), confirmOpts, nil, o, p.sweep, p.count)
	})
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Errorf("two attempts with a 1s retryDelay took %v — the sweep did not back off between them", elapsed)
	}
}

// ── waitVolumesDetached ──────────────────────────────────────────────────────

// volSeqClient answers ListVolumes from a script (one entry per call, the last
// repeating) so a test can drive the poll loop through several passes.
type volSeqClient struct {
	calls int
	steps []volStep
}

type volStep struct {
	vols []map[string]any
	err  error
}

func (c *volSeqClient) ListVolumes(context.Context) ([]map[string]any, error) {
	i := c.calls
	c.calls++
	if i >= len(c.steps) {
		i = len(c.steps) - 1
	}
	return c.steps[i].vols, c.steps[i].err
}

func attachedVol() []map[string]any {
	return []map[string]any{{"id": float64(1), "label": "pvc-a", "linode_id": float64(7)}}
}

func detachedVol() []map[string]any {
	return []map[string]any{{"id": float64(1), "label": "pvc-a", "linode_id": nil}}
}

// zeroDetachPoll removes the inter-poll pause for the duration of the test.
func zeroDetachPoll(t *testing.T) {
	t.Helper()
	prev := volumeDetachPollInterval
	volumeDetachPollInterval = 0
	t.Cleanup(func() { volumeDetachPollInterval = prev })
}

// The wait exists because Volumes detach asynchronously as the nodes tear down:
// it must keep polling until they do, within the budget it was given. Giving up
// on the first pass sends still-attached Volumes into a sweep that skips them,
// which is exactly how orphans survive a destroy.
func TestWaitVolumesDetachedPollsUntilTheyDetach(t *testing.T) {
	zeroDetachPoll(t)
	c := &volSeqClient{steps: []volStep{
		{vols: attachedVol()},
		{vols: attachedVol()},
		{vols: detachedVol()},
	}}
	out := captureStdout(t, func() {
		waitVolumesDetached(context.Background(), c, "1", 600)
	})

	if c.calls != 3 {
		t.Errorf("ListVolumes calls = %d, want 3 (poll until detached, inside the 600s budget)", c.calls)
	}
	if !strings.Contains(out, "all tracked Volumes are detached.") {
		t.Errorf("the converged case must say so:\n%s", out)
	}
	if strings.Contains(out, "still attached after 600s") {
		t.Errorf("gave up on the budget that had barely started:\n%s", out)
	}
	// The attempt counter is the only thing in the log that says how long this
	// waited, so it has to count up.
	for _, want := range []string{
		"tracked Volumes still attached: 1 (attempt 1)",
		"tracked Volumes still attached: 1 (attempt 2)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// A list error is NOT a count of zero and not a count of one: the loop has no
// idea how many are attached, and the log must say so rather than print a number
// it never read.
func TestWaitVolumesDetachedReportsAListErrorAsUnknown(t *testing.T) {
	zeroDetachPoll(t)
	c := &volSeqClient{steps: []volStep{
		{err: errors.New("500 internal server error")},
		{vols: detachedVol()},
	}}
	out := captureStdout(t, func() {
		waitVolumesDetached(context.Background(), c, "1", 600)
	})

	if !strings.Contains(out, "unknown (list error, attempt 1)") {
		t.Errorf("a failed list must report as unknown:\n%s", out)
	}
	if strings.Contains(out, "still attached: 1") {
		t.Errorf("reported a count it never read:\n%s", out)
	}
	if !strings.Contains(out, "all tracked Volumes are detached.") {
		t.Errorf("the list error is transient — the poll must continue:\n%s", out)
	}
}
