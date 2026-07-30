package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
)

// deploymentRolledOut's error guard had no test that could tell it apart from
// falling through to the parse. Existing coverage stubs kubectl to either succeed
// with a parseable count or fail with no output — and an empty string does not
// parse as rolled out either, so the guard could be deleted and everything stayed
// green.
//
// The case that separates them is a read that FAILS and still prints something
// parseable, which is exactly what kubectl does when it writes a partial answer
// and then loses the connection. That must count as "not rolled out": the harbor
// gate polls this on a 10-minute budget, and a transport failure read as a rollout
// ends the wait early on a registry that is not up.
func TestDeploymentRolledOutErrorVetoesTheParse(t *testing.T) {
	withKubectl(t, func(string) ([]byte, error) {
		return []byte("1/1"), errors.New("Unable to connect to the server: dial tcp: i/o timeout")
	})
	if deploymentRolledOut("harbor", "harbor-registry") {
		t.Fatal("a FAILED kubectl read must never count as a rollout — the error has to veto the parse, not fall through to it")
	}

	// ...and a clean read is still parsed, so the guard is not simply swallowing
	// every answer.
	withKubectl(t, func(string) ([]byte, error) { return []byte("2/2"), nil })
	if !deploymentRolledOut("harbor", "harbor-registry") {
		t.Fatal("a clean 2/2 read must count as rolled out")
	}
}

// The gate's deadline branch: with the budget spent, every harbor deployment is
// probed exactly once and the verb still returns nil — the registry wait is a SOFT
// gate (the convergence gate is the hard check), so a timeout must warn, not fail.
func TestRunCIWaitHarborDeadlineIsSoftAndProbesEachDeploymentOnce(t *testing.T) {
	origBudget := harborWaitBudget
	harborWaitBudget = 0
	t.Cleanup(func() { harborWaitBudget = origBudget })

	probes := 0
	withKubectl(t, func(a string) ([]byte, error) {
		if !strings.Contains(a, "get deployment") {
			return nil, errors.New("unexpected kubectl call: " + a)
		}
		probes++
		return []byte("0/1"), nil // never becomes available
	})
	if err := runCIWaitHarbor("", true); err != nil {
		t.Fatalf("a registry that never rolls out is a warning, not a failure: %v", err)
	}
	if want := len(health.HarborRegistryDeployments()); probes != want {
		t.Fatalf("probed %d times on a spent budget, want %d (one per harbor deployment); a gate that re-probes a deadline it has already missed burns the job's clock for nothing", probes, want)
	}
}
