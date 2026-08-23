package teardown

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// listVolumesFake answers ListVolumes from canned data OR a canned error.
// fakeTeardownClient (ci_teardown_test.go) can only succeed, and the list-error
// path is half of what waitVolumesDetached branches on.
type listVolumesFake struct {
	volumes   []map[string]any
	err       error
	calls     int
	detachErr error
	detached  []uint64
}

func (f *listVolumesFake) ListVolumes(context.Context) ([]map[string]any, error) {
	f.calls++
	return f.volumes, f.err
}

// DetachVolume records the call and, unless it is set to fail, actually
// detaches — so a caller that asks for the detach converges and one that only
// polls does not, which is the difference under test.
func (f *listVolumesFake) DetachVolume(_ context.Context, id uint64) error {
	f.detached = append(f.detached, id)
	if f.detachErr != nil {
		return f.detachErr
	}
	for _, v := range f.volumes {
		if uint64(v["id"].(float64)) == id {
			v["linode_id"] = nil
		}
	}
	return nil
}

// waitVolumesDetached reaches one of three verdicts per pass — "all detached",
// "gave up, sweep what you can", and the list-error case that must resolve to the
// second, never the first — and only the timeout one was asserted. All three are
// checked on a ZERO budget, so the immediate first check is what decides: no
// clock, no waiting, and an early return that stops being taken fails HERE instead
// of running out a budget nothing advances.
func TestWaitVolumesDetachedVerdicts(t *testing.T) {
	orig := volumeDetachPollInterval
	volumeDetachPollInterval = 0
	t.Cleanup(func() { volumeDetachPollInterval = orig })

	const (
		detachedVerdict = "all tracked Volumes are detached."
		gaveUpVerdict   = "sweeping what is detached"
	)

	t.Run("already detached wins before the deadline check", func(t *testing.T) {
		f := &listVolumesFake{volumes: []map[string]any{{"id": float64(1), "linode_id": nil}}}
		out := captureStdout(t, func() { waitVolumesDetached(context.Background(), f, "1", 0) })
		if !strings.Contains(out, detachedVerdict) {
			t.Fatalf("a set that is ALREADY detached must report so on the immediate first check, even on a zero budget; got:\n%s", out)
		}
		if strings.Contains(out, gaveUpVerdict) {
			t.Fatalf("nothing was still attached, so the gave-up path must not run; got:\n%s", out)
		}
	})

	t.Run("still attached at the deadline gives up and sweeps", func(t *testing.T) {
		f := &listVolumesFake{volumes: []map[string]any{{"id": float64(1), "linode_id": float64(7)}}}
		out := captureStdout(t, func() { waitVolumesDetached(context.Background(), f, "1", 0) })
		if !strings.Contains(out, gaveUpVerdict) {
			t.Fatalf("a Volume still attached at the deadline must fall through to the sweep; got:\n%s", out)
		}
		if strings.Contains(out, detachedVerdict) {
			t.Fatalf("an attached Volume must never be reported as detached; got:\n%s", out)
		}
	})

	t.Run("an unreadable volume list is never reported as detached", func(t *testing.T) {
		f := &listVolumesFake{err: errors.New("500 from /v4/volumes")}
		out := captureStdout(t, func() { waitVolumesDetached(context.Background(), f, "1", 0) })
		if strings.Contains(out, detachedVerdict) {
			t.Fatalf("a FAILED list says nothing about attachment — announcing 'all detached' turns an API outage into a clean bill of health in the teardown log, which is the one place an operator looks when Volumes survive a run; got:\n%s", out)
		}
		if !strings.Contains(out, gaveUpVerdict) {
			t.Fatalf("an unreadable list must land on the gave-up verdict; got:\n%s", out)
		}
		if f.calls != 1 {
			t.Fatalf("made %d list calls on a zero budget, want 1", f.calls)
		}
	})

	// The failure this closes: the LKE node reap stalls after a force-delete, so
	// tracked Volumes sit attached across every poll of the 600s window and the
	// destroy fails with orphans. Nothing was ever going
	// to detach them, because detachment was only ever a side effect of a reap
	// that had stopped making progress. Bound the budget with the poll interval
	// zeroed: a pass that merely watches never converges here.
	t.Run("a stalled detach is asked for, not only waited on", func(t *testing.T) {
		f := &listVolumesFake{volumes: []map[string]any{{"id": float64(1), "linode_id": float64(7)}}}
		out := captureStdout(t, func() { waitVolumesDetached(context.Background(), f, "1", 600) })
		if len(f.detached) == 0 {
			t.Fatalf("a Volume still attached inside the budget must have a detach REQUESTED — waiting alone cannot free a Volume whose node reap has stalled; got:\n%s", out)
		}
		if !strings.Contains(out, detachedVerdict) {
			t.Fatalf("once the detach lands the wait must converge, not run out its budget; got:\n%s", out)
		}
		if strings.Contains(out, gaveUpVerdict) {
			t.Fatalf("the detach succeeded, so the gave-up path must not run; got:\n%s", out)
		}
	})

	// A rejected detach must not abort the wait: the node reap may still finish on
	// its own, and the poll is what notices. Bounded by the step sequence, not by
	// the 600s budget — an unbounded spin here would fill the captured-stdout pipe
	// and hang the test rather than fail it.
	t.Run("a failing detach does not abort the wait", func(t *testing.T) {
		c := &volSeqClient{
			detachErr: errors.New("400 from /v4/volumes/1/detach"),
			steps: []volStep{
				{vols: attachedVol()},
				{vols: attachedVol()},
				{vols: detachedVol()},
			},
		}
		out := captureStdout(t, func() { waitVolumesDetached(context.Background(), c, "1", 600) })
		if len(c.detached) == 0 {
			t.Fatalf("the detach must still be attempted; got:\n%s", out)
		}
		if !strings.Contains(out, detachedVerdict) {
			t.Fatalf("a detach the API rejects must leave the poll running — the node reap can still finish on its own; got:\n%s", out)
		}
	})

	t.Run("untracked Volumes are ignored", func(t *testing.T) {
		// Someone else's attached Volume must not hold the wait open.
		f := &listVolumesFake{volumes: []map[string]any{{"id": float64(99), "linode_id": float64(7)}}}
		out := captureStdout(t, func() { waitVolumesDetached(context.Background(), f, "1", 0) })
		if !strings.Contains(out, detachedVerdict) {
			t.Fatalf("only tracked ids count toward 'still attached'; got:\n%s", out)
		}
	})
}
