package main

import (
	"encoding/json"
	"testing"
)

func TestPolicyKind(t *testing.T) {
	cases := map[string]string{
		`{}`:                                 "none",
		`{"deployment_branch_policy": null}`: "none",
		`{"deployment_branch_policy": {"custom_branch_policies": true}}`:  "custom",
		`{"deployment_branch_policy": {"protected_branches": true}}`:      "protected",
		`{"deployment_branch_policy": {"protected_branches": false}}`:     "none",
		`{"deployment_branch_policy": {"custom_branch_policies": false}}`: "none",
	}
	for in, want := range cases {
		var cfg map[string]any
		if err := json.Unmarshal([]byte(in), &cfg); err != nil {
			t.Fatalf("bad fixture %q: %v", in, err)
		}
		if got := policyKind(cfg); got != want {
			t.Errorf("policyKind(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestEnvCfgCoercers(t *testing.T) {
	if numOr(float64(7), 0) != 7 || numOr("x", 3) != 3 || numOr(nil, 0) != 0 {
		t.Error("numOr")
	}
	if !boolOr(true, false) || boolOr("x", true) != true || boolOr(nil, false) {
		t.Error("boolOr")
	}
	if len(sliceOr([]any{1, 2})) != 2 || len(sliceOr(nil)) != 0 || len(sliceOr("x")) != 0 {
		t.Error("sliceOr")
	}
}

func TestIsPlanLimitErr(t *testing.T) {
	// The exact message GitHub returns when a private repo's plan lacks
	// environment protection rules.
	planErr := "gh: Failed to create the environment protection rule. Please ensure the " +
		"billing plan supports the required reviewers protection rule. (HTTP 422)"
	if !isPlanLimitErr(planErr) {
		t.Error("want plan-limit message classified as plan limit")
	}
	for _, other := range []string{
		"HTTP 404: Not Found",
		`is not of type "boolean"`,
		"name has already been taken",
		"",
	} {
		if isPlanLimitErr(other) {
			t.Errorf("unrelated error misclassified as plan limit: %q", other)
		}
	}
}
