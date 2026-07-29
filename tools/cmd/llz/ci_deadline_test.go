package main

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
	volumes []map[string]any
	err     error
	calls   int
}

func (f *listVolumesFake) ListVolumes(context.Context) ([]map[string]any, error) {
	f.calls++
	return f.volumes, f.err
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

	t.Run("untracked Volumes are ignored", func(t *testing.T) {
		// Someone else's attached Volume must not hold the wait open.
		f := &listVolumesFake{volumes: []map[string]any{{"id": float64(99), "linode_id": float64(7)}}}
		out := captureStdout(t, func() { waitVolumesDetached(context.Background(), f, "1", 0) })
		if !strings.Contains(out, detachedVerdict) {
			t.Fatalf("only tracked ids count toward 'still attached'; got:\n%s", out)
		}
	})
}
