package kubectlprobe

import (
	"errors"
	"testing"
)

// Reachable moved here from internal/converge without a test.
//
// It exists because `llz ci diagnose-argocd` and health-sla both GATE on it: if it
// wrongly reports reachable, every downstream probe runs against a cluster that
// cannot answer and the diagnostics come back empty. If it wrongly reports
// unreachable, a healthy cluster is reported as torn down. Both are silent.
func TestReachable(t *testing.T) {
	orig := Exec
	t.Cleanup(func() { Exec = orig })

	var gotName string
	var gotArgs []string
	Exec = func(name string, args ...string) ([]byte, error) {
		gotName, gotArgs = name, args
		return nil, nil
	}
	if !Reachable() {
		t.Error("an apiserver that answers must read as reachable")
	}
	if gotName != "kubectl" {
		t.Errorf("probed %q, want kubectl", gotName)
	}
	// The timeout is load-bearing: without it an unreachable apiserver hangs the
	// gate instead of failing it.
	var timed bool
	for _, a := range gotArgs {
		if a == "--request-timeout=10s" {
			timed = true
		}
	}
	if !timed {
		t.Errorf("probe argv %v carries no --request-timeout; an unreachable apiserver would "+
			"hang the gate rather than fail it", gotArgs)
	}

	Exec = func(string, ...string) ([]byte, error) { return nil, errors.New("connection refused") }
	if Reachable() {
		t.Error("a refused connection must read as unreachable")
	}
}
