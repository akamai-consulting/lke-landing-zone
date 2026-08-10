package converge

// Gap-closing tests for ci_wait_apl_pipeline.go surfaced by mutation testing.
// The existing suite drives every stage to an IMMEDIATE success, which makes the
// existence budget and the poll cadence invisible: a stage whose budget collapsed
// to zero, or whose 10s cadence collapsed to zero, still passes when the first
// probe already answers. On a real bootstrap the first probe never answers — the
// helmfile takes 10-15 minutes — so those are exactly the values that decide
// whether this gate can tell "coming up" from "stalled".

import (
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
)

// aplPollDeps builds gate seams with a clock advanced BY SLEEP (never on a bare
// read), so a test observes the poll cadence the gate actually asks for. The
// returned pointer is the fake clock, for asserting how much time a wait consumed.
func aplPollDeps(kubectl cigate.Runner) (cigate.Deps, *time.Time) {
	now := time.Unix(1_700_000_000, 0)
	d := cigate.Deps{
		Kubectl: kubectl,
		Now:     func() time.Time { return now },
		Sleep:   func(step time.Duration) { now = now.Add(step) },
	}
	return d, &now
}

// Every stage's existBudget must be big enough to survive a resource that is not
// there on the first probe — the normal case, since apl-operator's helmfile
// installs these over minutes. A stage whose budget is effectively zero gets one
// probe and declares the helmfile stalled on a cluster that is merely starting.
func TestAplPipelineStagesToleratePreCreationProbes(t *testing.T) {
	stages := aplPipelineStages()
	if len(stages) == 0 {
		t.Fatal("no stages — the gate would pass having waited for nothing")
	}
	for i, s := range stages {
		t.Run(s.desc, func(t *testing.T) {
			if s.existBudget < time.Minute {
				t.Fatalf("stage %q existBudget = %v, want >= 1m — apl-operator's helmfile needs minutes to produce %s",
					s.desc, s.existBudget, s.resource)
			}
			misses := 0
			d, _ := aplPollDeps(func(args ...string) (string, bool) {
				joined := strings.Join(args, " ")
				// This stage's resource is absent for the first three polls
				// (30s at the 10s cadence), then lands. Everything else answers.
				if strings.Contains(joined, "get "+s.resource) {
					misses++
					return "", misses > 3
				}
				return "", true
			})
			if err := waitAplPipeline(stages[i:i+1], d); err != nil {
				t.Fatalf("stage %q failed after %d absent probes: %v — its existence budget cannot ride out a resource that is not created yet",
					s.desc, misses, err)
			}
		})
	}
}

// The existence poll must advance the clock by its 10s cadence. A cadence of zero
// keeps the deadline arithmetic technically correct while hammering the apiserver
// as fast as the process can loop — and, with any real clock, spends the whole
// budget in a hot loop instead of the intended ~60 polls.
func TestWaitForAplResourceExistencePollUsesTheTenSecondCadence(t *testing.T) {
	misses := 0
	d, clock := aplPollDeps(func(args ...string) (string, bool) {
		if strings.Contains(strings.Join(args, " "), "get crd/slow.example.io") {
			misses++
			return "", misses > 3 // appears on the 4th probe → 3 sleeps
		}
		return "", true
	})
	start := *clock
	st := aplWaitStage{
		desc: "slow CRD", resource: "crd/slow.example.io", forClause: "Established",
		existBudget: 600 * time.Second, condTimeout: "3m",
	}
	if err := waitForAplResource(d, st); err != nil {
		t.Fatalf("waitForAplResource = %v, want nil", err)
	}
	if got, want := clock.Sub(start), 30*time.Second; got != want {
		t.Errorf("existence poll consumed %v across 3 retries, want %v — the 10s cadence is not being slept", got, want)
	}
}

// An existence timeout's diagnostics are the only thing that explains WHY the
// helmfile stalled. Making the calls is not enough: their OUTPUT has to reach
// stderr. (The existing suite asserts only that the log call was made.)
func TestDumpAplOperatorDiagnosticsPrintsWhatItReads(t *testing.T) {
	const pods = "apl-operator-6d9f-xyz   0/1   CrashLoopBackOff   7   12m"
	const logs = "Error: could not clone values repo: authentication required"
	d, _ := aplPollDeps(func(args ...string) (string, bool) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "get pods"):
			return pods + "\n", true
		case strings.Contains(joined, "logs deploy/apl-operator"):
			return logs + "\n", true
		}
		return "", true
	})
	out := captureStderr(t, func() { dumpAplOperatorDiagnostics(d) })
	if !strings.Contains(out, pods) {
		t.Errorf("apl-operator pod state never reached stderr:\n%s", out)
	}
	if !strings.Contains(out, logs) {
		t.Errorf("apl-operator logs never reached stderr — the stall reason is invisible:\n%s", out)
	}
}

// ...and the same output must ride the real failure path, so an operator reading
// a stalled bootstrap sees the reason next to the error.
func TestWaitAplPipelineExistenceTimeoutSurfacesOperatorLogs(t *testing.T) {
	const logs = "Error: helmfile apply failed on chart argocd"
	stages := aplPipelineStages()
	// A clock that also advances on a bare read, so the budget expires no matter
	// what the poll cadence is (this test must fail, never hang).
	now, _ := fakeClock(1000 * time.Second)
	d := cigate.Deps{
		Now:   now,
		Sleep: func(time.Duration) {},
		Kubectl: func(args ...string) (string, bool) {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "get "+stages[0].resource):
				return "", false // never appears
			case strings.Contains(joined, "logs deploy/apl-operator"):
				return logs + "\n", true
			}
			return "", true
		},
	}
	var err error
	out := captureStderr(t, func() { err = waitAplPipeline(stages, d) })
	if err == nil {
		t.Fatal("a CRD that never appears must fail the gate loud (convergence contract)")
	}
	if !strings.Contains(out, logs) {
		t.Errorf("the existence-timeout diagnostics did not carry the operator logs:\n%s", out)
	}
}
