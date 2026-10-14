package assertplatform

import (
	"strings"
	"testing"
	"time"
)

// The existence poll's CADENCE had no coverage. Every existing assertArgoApp test
// drives the loop by kubectl call count, so the sleep argument could be anything —
// including zero — and they would all still pass. A zero interval is not a slow
// gate, it is a hot loop against the argocd apiserver that also freezes the fake
// clock, so the deadline this gate exists to report never arrives.
//
// The app appears on the 4th existence probe, so the loop terminates on call count
// no matter what the interval is; what is asserted is the three waits in between.
func TestAssertArgoAppPollsOnATenSecondCadence(t *testing.T) {
	p := newPollRecorder()
	probes := 0
	d := p.deps(func(args ...string) (string, bool) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "get application.argoproj.io platform-openbao"):
			probes++
			return "", probes >= 4
		case strings.Contains(joined, "ComparisonError"):
			return "", true // no comparison error → no hard refresh, no git-auth path
		default:
			return "Running\tsyncing wave -15", true // parent is syncing, not terminal
		}
	})
	if err := assertArgoApp(d, "argocd", "platform-openbao", "platform-bootstrap", 30*time.Minute); err != nil {
		t.Fatalf("the app appeared on the 4th probe but the gate failed: %v", err)
	}
	if probes != 4 {
		t.Fatalf("made %d existence probes, want 4", probes)
	}
	p.wantEveryPollAt(t, 10*time.Second, 3)
	// And the clock actually moved — which is what makes the deadline reachable.
	if got := p.elapsed(); got != 30*time.Second {
		t.Fatalf("clock advanced %s across 3 polls, want 30s; a wait that does not move the clock can never reach the gate's deadline", got)
	}
}

// The deadline branch on an ADVANCING clock: an app that never appears must burn
// its budget at the poll cadence and then fail, naming the app and the budget.
// The existing deadline tests use a clock driven by now() reads, which reaches the
// deadline no matter how (or whether) the loop waits; this one reaches it only by
// sleeping, which is what the gate does in production.
func TestAssertArgoAppDeadlineIsSpentAtThePollCadence(t *testing.T) {
	p := newPollRecorder()
	d := p.deps(func(args ...string) (string, bool) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "get application.argoproj.io platform-openbao"):
			return "", false // never created
		case strings.Contains(joined, "ComparisonError"):
			return "", true
		default:
			return "Running\tsyncing wave -15", true
		}
	})
	err := assertArgoApp(d, "argocd", "platform-openbao", "platform-bootstrap", 40*time.Second)
	if err == nil || !strings.Contains(err.Error(), "not created within 40s") {
		t.Fatalf("err = %v, want the deadline verdict naming the app and the budget — this is the line a CI operator reads when the gate gives up", err)
	}
	if !strings.Contains(err.Error(), "platform-openbao") {
		t.Fatalf("the deadline error must name the missing Application, got %v", err)
	}
	// Probes at t=0,10,20,30,40; the one landing exactly on the deadline is the
	// last tried, so four waits happen before the gate gives up.
	p.wantEveryPollAt(t, 10*time.Second, 4)
}
