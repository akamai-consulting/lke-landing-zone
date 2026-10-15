package tokeninv

// TestCapitalizeFirst followed capitalizeFirst here; it was in package main's
// coverage_tier1_test.go, another file named for a coverage tier rather than a
// subject.

import (
	"reflect"
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

// InputsFromLookup is the env→struct reader the release-e2e dry-run fence runs
// the delivered Setup env through. Every field must come from SOME variable — a
// field left unread is one the fence silently feeds as "".
func TestInputsFromLookupReadsEveryField(t *testing.T) {
	seen := map[string]bool{}
	in := InputsFromLookup(func(k string) string { seen[k] = true; return "v-" + k })
	v := reflect.ValueOf(in)
	for i := 0; i < v.NumField(); i++ {
		if got := v.Field(i).String(); !strings.HasPrefix(got, "v-") {
			t.Errorf("RotationInputs.%s is not read from the environment (got %q)", v.Type().Field(i).Name, got)
		}
	}
	if len(seen) != v.NumField() {
		t.Errorf("%d fields but %d distinct env vars read: %v", v.NumField(), len(seen), seen)
	}
	t.Setenv("PAT_APPLY", "true")
	if InputsFromEnv().PATApply != "true" {
		t.Error("InputsFromEnv must be InputsFromLookup over the process environment")
	}
}
