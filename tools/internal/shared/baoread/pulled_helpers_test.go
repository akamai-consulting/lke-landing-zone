package baoread

// pulled_helpers_test.go — assertion helpers the moved exec tests use, copied
// from package main. Copied, not shared: each is a `*testing.T` helper.

import (
	"testing"
	"time"
)

// wantEverySleepAt is wantEveryPollAt for the loops that take a plain
// func(time.Duration) sleep seam rather than aplGateDeps (baoread.WaitForState).
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
