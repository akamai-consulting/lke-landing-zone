package tokeninv

// TestCapitalizeFirst followed capitalizeFirst here; it was in package main's
// coverage_tier1_test.go, another file named for a coverage tier rather than a
// subject.

import (
	"strings"
	"testing"
)

func TestCapitalizeFirst(t *testing.T) {
	for in, want := range map[string]string{"": "", "hello": "Hello", "A": "A", "123": "123"} {
		if got := capitalizeFirst(in); got != want {
			t.Errorf("capitalizeFirst(%q) = %q, want %q", in, got, want)
		}
	}
}

// RouteOutputs is the exported view the release-e2e dry-run coupling test reads.
// It must be the SAME lines RunRotationPlan writes — a copy that drifted would
// let that test prove a dry run of a router nobody runs.
func TestRouteOutputsIsTheRoutedPlan(t *testing.T) {
	in := RotationInputs{Event: "workflow_dispatch", Scope: "linode-pat", Confirm: "rotate:linode-pat",
		Reason: "r", PATApply: "false", Deployments: `["e2e"]`}
	got, err := RouteOutputs(in)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := routeRotation(in)
	if strings.Join(got, "\n") != strings.Join(p.outputLines(), "\n") {
		t.Fatalf("RouteOutputs diverged from the plan's own output lines:\n%v\nvs\n%v", got, p.outputLines())
	}
	if _, err := RouteOutputs(RotationInputs{Event: "workflow_dispatch", Scope: "linode-pat", Confirm: "wrong", Reason: "r"}); err == nil {
		t.Fatal("a refused route must surface its error, not an empty plan")
	}
}
