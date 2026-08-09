package assertplatform

// A COPY of package main's pollRecorder fixture, for the same reason
// kubectlfake_test.go is a copy: a fixture shared across an extraction boundary
// makes the extracted package depend on the CLI it was extracted from.

import (
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
)

// pollRecorderEpoch is the fake clock's zero point — a fixed instant so a failure
// message reads the same on every machine.
var pollRecorderEpoch = time.Unix(1_700_000_000, 0)

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

// pollRecorderUnfreeze is how far the clock jumps when the code under test asks
// to sleep for a non-positive interval — never freeze, so a zero interval fails
// an assertion instead of hanging the suite.
const pollRecorderUnfreeze = time.Hour
