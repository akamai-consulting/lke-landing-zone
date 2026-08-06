package main

// ci_shared_deadline_test.go supplies the seam the gate tests were missing: a
// fake clock that ADVANCES BY WHAT THE CODE UNDER TEST SLEEPS, and that RECORDS
// the interval it was asked to sleep for.
//
// The suite already had two clock stubs and neither one covered this:
//
//	sleep: func(d time.Duration) { now = now.Add(d) }  // advances, but the
//	                                                   // interval is discarded
//	sleep: func(time.Duration) {}                      // never advances at all
//
// Discarding the interval is what let `d.Sleep(10 * time.Second)` degrade to
// `d.Sleep(10 / time.Second)` unnoticed: that is integer division of 10 by 1e9,
// i.e. a ZERO interval. Every existing assertion stays color.Green, because they all
// drive their loops by kubectl call count. In production a zero interval turns a
// polite 10s poll into a hot spin against the apiserver; under a fake clock that
// only moves when something sleeps, it freezes time outright, so the deadline
// never arrives and the gate never gives up.
//
// pollRecorder pins the cadence itself, and its sleep stub refuses to be frozen:
// a zero interval jumps the clock forward instead of standing still, so the test
// FAILS on the recorded interval rather than hanging the suite.

import (
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
)

// pollRecorderEpoch is the fake clock's zero point — a fixed instant so a failure
// message reads the same on every machine.
var pollRecorderEpoch = time.Unix(1_700_000_000, 0)

// pollRecorderUnfreeze is how far the clock jumps when the code under test asks
// to sleep for a non-positive interval. It only has to outrun the budgets the
// tests below use (all well under an hour) so that the loop reaches its deadline
// and the assertion — not the test timeout — reports the fault.
const pollRecorderUnfreeze = time.Hour

type pollRecorder struct {
	now    time.Time
	sleeps []time.Duration
}

func newPollRecorder() *pollRecorder { return &pollRecorder{now: pollRecorderEpoch} }

// deps wires the recorder's clock into aplGateDeps behind the given kubectl script.
func (p *pollRecorder) deps(kubectl cigate.Runner) cigate.Deps {
	return cigate.Deps{
		Kubectl: kubectl,
		Now:     func() time.Time { return p.now },
		Sleep: func(d time.Duration) {
			p.sleeps = append(p.sleeps, d)
			if d <= 0 {
				p.now = p.now.Add(pollRecorderUnfreeze)
				return
			}
			p.now = p.now.Add(d)
		},
	}
}

// elapsed is how far the recorder's clock has moved since the epoch.
func (p *pollRecorder) elapsed() time.Duration { return p.now.Sub(pollRecorderEpoch) }

// wantEveryPollAt asserts the loop waited exactly count times, every wait for
// exactly interval. A collapsed (zero) or drifting interval fails here.
func (p *pollRecorder) wantEveryPollAt(t *testing.T, interval time.Duration, count int) {
	t.Helper()
	for i, d := range p.sleeps {
		if d != interval {
			t.Fatalf("poll %d waited %s, want %s — the poll interval collapsed; a zero interval spins the loop against the apiserver and stops the clock, so the deadline never arrives (waits: %v)",
				i+1, d, interval, p.sleeps)
		}
	}
	if len(p.sleeps) != count {
		t.Fatalf("polled %d times at %s, want %d — the loop is not running on its documented cadence (waits: %v)",
			len(p.sleeps), interval, count, p.sleeps)
	}
}

// wantEverySleepAt is wantEveryPollAt for the loops that take a plain
// func(time.Duration) sleep seam rather than aplGateDeps (waitForBaoState).
func wantEverySleepAt(t *testing.T, slept []time.Duration, interval time.Duration, count int) {
	t.Helper()
	for i, d := range slept {
		if d != interval {
			t.Fatalf("wait %d was %s, want %s — the poll interval collapsed; a zero interval makes the elapsed counter stand still, so the budget is never spent and the wait never returns (waits: %v)",
				i+1, d, interval, slept)
		}
	}
	if len(slept) != count {
		t.Fatalf("waited %d times at %s, want %d (waits: %v)", len(slept), interval, count, slept)
	}
}
