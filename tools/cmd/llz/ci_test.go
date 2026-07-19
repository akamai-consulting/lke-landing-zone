package main

// Unit tests for the shared destroy-path sweep loop in ci.go. sweepUntilEmpty is
// what stands between a half-finished teardown and orphaned Volumes /
// NodeBalancers blocking the NEXT apply's preflight, so the retry, gating and
// terminal-error behavior are pinned here rather than left to the live CI job.
//
// The loop only touches its *linode.Client to build the ciDeleter closure, so a
// sweep body that deletes nothing can pass a nil client and run offline.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// sweepProbe counts how many passes the loop drove and serves canned verify
// counts, one per attempt (the last entry repeats once exhausted).
type sweepProbe struct {
	sweeps    int
	counts    int
	remaining []int
	countErr  error
	sweepErr  error
}

func (p *sweepProbe) sweep(func(path, desc string)) error {
	p.sweeps++
	return p.sweepErr
}

func (p *sweepProbe) count() (int, error) {
	p.counts++
	if p.countErr != nil {
		return -1, p.countErr
	}
	i := p.counts - 1
	if i >= len(p.remaining) {
		i = len(p.remaining) - 1
	}
	return p.remaining[i], nil
}

// testSweepOpts is the wording block with retryDelay zeroed so the retries do
// not actually sleep.
func testSweepOpts(attempts int, requireEmpty bool) sweepOpts {
	return sweepOpts{
		cmd:          "reap-widgets",
		banner:       "=== orphan Widgets",
		singular:     "Widget",
		plural:       "Widgets",
		unit:         "tracked Widget(s)",
		goneMsg:      "verified: all tracked Widgets are gone.",
		attempts:     attempts,
		retryDelay:   0,
		requireEmpty: requireEmpty,
	}
}

// confirmOpts is the --yes, non-dry-run combination: the only one under which
// the verify/retry half of the loop engages.
var confirmOpts = globalOpts{yes: true}

func TestSweepUntilEmptyVerifiesEmptyOnFirstPass(t *testing.T) {
	p := &sweepProbe{remaining: []int{0}}
	err := sweepUntilEmpty(context.Background(), confirmOpts, nil, testSweepOpts(4, true), p.sweep, p.count)
	if err != nil {
		t.Fatalf("sweepUntilEmpty = %v, want nil", err)
	}
	if p.sweeps != 1 {
		t.Errorf("sweeps = %d, want 1 (a verified-empty first pass must not retry)", p.sweeps)
	}
	if p.counts != 1 {
		t.Errorf("counts = %d, want 1", p.counts)
	}
}

func TestSweepUntilEmptyRetriesUntilEmpty(t *testing.T) {
	// Two survivors, then one, then gone on the third verify.
	p := &sweepProbe{remaining: []int{2, 1, 0}}
	err := sweepUntilEmpty(context.Background(), confirmOpts, nil, testSweepOpts(4, true), p.sweep, p.count)
	if err != nil {
		t.Fatalf("sweepUntilEmpty = %v, want nil", err)
	}
	if p.sweeps != 3 {
		t.Errorf("sweeps = %d, want 3 (retry until the verify reports zero)", p.sweeps)
	}
}

func TestSweepUntilEmptyFailsWhenOrphansRemain(t *testing.T) {
	p := &sweepProbe{remaining: []int{3}}
	err := sweepUntilEmpty(context.Background(), confirmOpts, nil, testSweepOpts(3, true), p.sweep, p.count)
	if err == nil {
		t.Fatal("sweepUntilEmpty = nil, want the orphans-remain error")
	}
	if p.sweeps != 3 {
		t.Errorf("sweeps = %d, want 3 (every attempt must be spent)", p.sweeps)
	}
	want := "reap-widgets: 3 tracked Widget(s) still present after 3 attempt(s) — orphans remain; " +
		"failing the destroy so they don't block the next apply's preflight"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestSweepUntilEmptyCountErrorReportsSome(t *testing.T) {
	// A failing verify leaves the -1 sentinel, which must render as "some"
	// rather than a bogus negative count — the destroy still fails.
	p := &sweepProbe{countErr: errors.New("boom")}
	err := sweepUntilEmpty(context.Background(), confirmOpts, nil, testSweepOpts(2, true), p.sweep, p.count)
	if err == nil {
		t.Fatal("sweepUntilEmpty = nil, want the orphans-remain error")
	}
	if !strings.HasPrefix(err.Error(), "reap-widgets: some tracked Widget(s) still present after 2 attempt(s)") {
		t.Errorf("error = %q, want a 'some' count", err.Error())
	}
	if p.sweeps != 2 {
		t.Errorf("sweeps = %d, want 2", p.sweeps)
	}
}

func TestSweepUntilEmptySinglePassWithoutRequireEmpty(t *testing.T) {
	// Historical best-effort behavior: no --require-empty means one pass and no
	// verification at all, even with attempts to spare.
	p := &sweepProbe{remaining: []int{5}}
	err := sweepUntilEmpty(context.Background(), confirmOpts, nil, testSweepOpts(4, false), p.sweep, p.count)
	if err != nil {
		t.Fatalf("sweepUntilEmpty = %v, want nil", err)
	}
	if p.sweeps != 1 {
		t.Errorf("sweeps = %d, want 1", p.sweeps)
	}
	if p.counts != 0 {
		t.Errorf("counts = %d, want 0 (no verify without --require-empty)", p.counts)
	}
}

func TestSweepUntilEmptySinglePassWhenNotConfirmed(t *testing.T) {
	// Dry-run deleted nothing, so re-verifying and retrying would burn the whole
	// attempt budget confirming what was never attempted.
	for _, tc := range []struct {
		name string
		g    globalOpts
	}{
		{"no --yes", globalOpts{}},
		{"--yes with --dry-run", globalOpts{yes: true, dryRun: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &sweepProbe{remaining: []int{5}}
			err := sweepUntilEmpty(context.Background(), tc.g, nil, testSweepOpts(4, true), p.sweep, p.count)
			if err != nil {
				t.Fatalf("sweepUntilEmpty = %v, want nil", err)
			}
			if p.sweeps != 1 || p.counts != 0 {
				t.Errorf("sweeps=%d counts=%d, want 1 and 0", p.sweeps, p.counts)
			}
		})
	}
}

func TestSweepUntilEmptySweepErrorAborts(t *testing.T) {
	// A sweep that cannot enumerate has nothing to converge on: fail out rather
	// than retry into the same error.
	p := &sweepProbe{remaining: []int{1}, sweepErr: errors.New("list Widgets: boom")}
	err := sweepUntilEmpty(context.Background(), confirmOpts, nil, testSweepOpts(4, true), p.sweep, p.count)
	if err == nil || err.Error() != "list Widgets: boom" {
		t.Fatalf("sweepUntilEmpty = %v, want the sweep error verbatim", err)
	}
	if p.sweeps != 1 {
		t.Errorf("sweeps = %d, want 1 (abort on the first sweep error)", p.sweeps)
	}
}

func TestSweepUntilEmptyNormalizesAttempts(t *testing.T) {
	// attempts <= 0 must still run exactly one pass, not zero (a zero-pass loop
	// would fall straight through to the terminal error having deleted nothing).
	for _, attempts := range []int{0, -3} {
		p := &sweepProbe{remaining: []int{1}}
		err := sweepUntilEmpty(context.Background(), confirmOpts, nil, testSweepOpts(attempts, true), p.sweep, p.count)
		if err == nil {
			t.Fatalf("attempts=%d: want the orphans-remain error", attempts)
		}
		if !strings.Contains(err.Error(), "after 1 attempt(s)") {
			t.Errorf("attempts=%d: error = %q, want it normalized to 1 attempt", attempts, err.Error())
		}
		if p.sweeps != 1 {
			t.Errorf("attempts=%d: sweeps = %d, want 1", attempts, p.sweeps)
		}
	}
}
