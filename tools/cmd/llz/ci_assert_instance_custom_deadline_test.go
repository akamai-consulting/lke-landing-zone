package main

import (
	"strings"
	"testing"
	"time"
)

// assertInstanceCustom has TWO poll loops under ONE deadline, and neither one's
// cadence was pinned: the existing tests advance past both phases on kubectl call
// count, so the interval each loop waits could be anything — including zero, which
// stops the shared fake clock dead and leaves the single `within` deadline
// unreachable from either phase.
//
// Both phases converge on their 3rd read here, so the loops terminate on call
// count regardless of the interval; what is asserted is the four waits in between.
func TestAssertInstanceCustomPollsBothPhasesOnATenSecondCadence(t *testing.T) {
	p := newPollRecorder()
	exists, reads := 0, 0
	d := p.deps(func(args ...string) (string, bool) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasSuffix(joined, "-o json"): // phase 2: sync/health read
			reads++
			if reads >= 3 {
				return appStatusJSON("Synced", "Healthy"), true
			}
			return appStatusJSON("OutOfSync", "Missing"), true
		case strings.Contains(joined, "get application.argoproj.io instance-custom-"):
			exists++ // phase 1: existence probe
			return "", exists >= 3
		default:
			return "", true
		}
	})
	if err := assertInstanceCustom(d, "llz-e2e-custom", "instance-custom", time.Hour); err != nil {
		t.Fatalf("the hatch converged but the gate failed: %v", err)
	}
	if exists != 3 || reads != 3 {
		t.Fatalf("existence probes = %d, sync/health reads = %d, want 3 and 3", exists, reads)
	}
	// Two waits in phase 1 (after probes 1 and 2) plus two in phase 2.
	p.wantEveryPollAt(t, 10*time.Second, 4)
	if got := p.elapsed(); got != 40*time.Second {
		t.Fatalf("clock advanced %s across 4 polls, want 40s; waits that do not move the clock make the shared `within` deadline unreachable", got)
	}
}

// Phase 1's deadline branch on an ADVANCING clock: an App the ApplicationSet never
// generates must spend the whole budget at the poll cadence, then fail naming the
// App and the budget — and must never reach the sync/health read.
func TestAssertInstanceCustomPhase1DeadlineIsSpentAtThePollCadence(t *testing.T) {
	p := newPollRecorder()
	syncRead := false
	d := p.deps(func(args ...string) (string, bool) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasSuffix(joined, "-o json"):
			syncRead = true
			return appStatusJSON("Synced", "Healthy"), true
		case strings.Contains(joined, "get applicationset instance-custom"):
			return "[ErrorOccurred: True unable to generate: repository not found]", true
		default:
			return "", false // the generated Application never exists
		}
	})
	err := assertInstanceCustom(d, "llz-e2e-custom", "instance-custom", 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "not generated within 30s") {
		t.Fatalf("err = %v, want the phase-1 deadline verdict naming the App and the budget", err)
	}
	if syncRead {
		t.Fatal("an App that never appeared must not be probed for sync/health")
	}
	// Probes at t=0,10,20,30 — the probe landing exactly on the deadline is the
	// last one tried, so three waits precede the verdict.
	p.wantEveryPollAt(t, 10*time.Second, 3)
}

// Phase 2's deadline branch on an ADVANCING clock: an App stuck OutOfSync must
// spend the REMAINING budget at the poll cadence — the deadline is shared with
// phase 1, so time already burned looking for the App is not handed back.
func TestAssertInstanceCustomPhase2DeadlineIsSpentAtThePollCadence(t *testing.T) {
	p := newPollRecorder()
	d := p.deps(func(args ...string) (string, bool) {
		joined := strings.Join(args, " ")
		if strings.HasSuffix(joined, "-o json") {
			return appStatusJSON("OutOfSync", "Missing"), true // never converges
		}
		return "", true // the App exists immediately
	})
	err := assertInstanceCustom(d, "llz-e2e-custom", "instance-custom", 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "not Synced+Healthy within 30s") {
		t.Fatalf("err = %v, want the phase-2 deadline verdict", err)
	}
	if !strings.Contains(err.Error(), "sync=OutOfSync") {
		t.Fatalf("the deadline error must carry the last observed sync/health, got %v", err)
	}
	p.wantEveryPollAt(t, 10*time.Second, 3)
}
