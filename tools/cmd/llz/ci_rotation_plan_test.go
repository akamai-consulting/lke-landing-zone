package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRouteRotationSchedules(t *testing.T) {
	monthly, err := routeRotation(rotationInputs{
		Event: "schedule", Cron: cronMonthlyRotate, Deployments: `["primary","secondary"]`,
	})
	if err != nil {
		t.Fatalf("monthly: %v", err)
	}
	if !monthly.RunLKEAdmin || !monthly.RunPATCreate || !monthly.RunTFStateCreate ||
		!monthly.PATApply || !monthly.TFStateApply {
		t.Errorf("monthly should arm lke-admin + PAT create + TF-state create: %+v", monthly)
	}
	if monthly.RunPATRevoke || monthly.RunTFStateRevoke || monthly.RunPATPropagateOnly {
		t.Errorf("monthly must not run the revoke reapers: %+v", monthly)
	}
	if monthly.Regions != `["primary","secondary"]` {
		t.Errorf("monthly regions = %q, want the discovered deployments", monthly.Regions)
	}

	daily, err := routeRotation(rotationInputs{Event: "schedule", Cron: cronDailyRevoke})
	if err != nil {
		t.Fatalf("daily: %v", err)
	}
	if !daily.RunPATRevoke || !daily.RunTFStateRevoke || !daily.RevokeApply || !daily.TFStateRevokeApply {
		t.Errorf("daily should arm both revoke reapers: %+v", daily)
	}
	if daily.RunLKEAdmin || daily.RunPATCreate || daily.RunTFStateCreate {
		t.Errorf("daily must not create anything: %+v", daily)
	}
	if daily.Regions != "[]" {
		t.Errorf("daily regions = %q, want []", daily.Regions)
	}

	if _, err := routeRotation(rotationInputs{Event: "schedule", Cron: "0 0 * * *"}); err == nil {
		t.Error("unknown cron must be refused")
	}
}

func TestRouteRotationDispatchScopes(t *testing.T) {
	base := rotationInputs{Event: "workflow_dispatch", Reason: "incident 42",
		Region: "primary", Deployments: `["primary"]`}

	// Every scope refuses a wrong confirm phrase.
	for scope, confirm := range map[string]string{
		"lke-admin":                 "rotate:primary",
		"linode-pat":                "rotate:linode-pat",
		"linode-pat-propagate-only": "rotate:linode-pat-propagate-only",
		"linode-pat-revoke":         "rotate:linode-pat-revoke",
		"tf-state-key":              "rotate:tf-state-key",
		"tf-state-key-revoke":       "rotate:tf-state-key-revoke",
		"all":                       "rotate:all",
	} {
		in := base
		in.Scope, in.Confirm = scope, "wrong"
		if _, err := routeRotation(in); err == nil || !strings.Contains(err.Error(), confirm) {
			t.Errorf("scope %s with wrong confirm: err=%v, want mismatch naming %q", scope, err, confirm)
		}
		in.Confirm = confirm
		if _, err := routeRotation(in); err != nil {
			t.Errorf("scope %s with exact confirm: %v", scope, err)
		}
	}

	// lke-admin scopes to the one region.
	in := base
	in.Scope, in.Confirm = "lke-admin", "rotate:primary"
	p, _ := routeRotation(in)
	if !p.RunLKEAdmin || p.Regions != `["primary"]` {
		t.Errorf("lke-admin plan = %+v", p)
	}

	// Apply flags pass through only when the input is the literal "true".
	in = base
	in.Scope, in.Confirm, in.PATApply = "linode-pat", "rotate:linode-pat", "true"
	if p, _ = routeRotation(in); !p.RunPATCreate || !p.PATApply {
		t.Errorf("armed linode-pat plan = %+v", p)
	}
	in.PATApply = ""
	if p, _ = routeRotation(in); p.PATApply {
		t.Errorf("unset PAT_APPLY must stay false: %+v", p)
	}

	// all: everything except propagate-only, fanned over the deployments.
	in = base
	in.Scope, in.Confirm = "all", "rotate:all"
	p, _ = routeRotation(in)
	if !(p.RunLKEAdmin && p.RunPATCreate && p.RunPATRevoke && p.RunTFStateCreate && p.RunTFStateRevoke) ||
		p.RunPATPropagateOnly || p.Regions != `["primary"]` {
		t.Errorf("all plan = %+v", p)
	}

	// Blank reason and unknown scope are refused.
	in = base
	in.Scope, in.Confirm, in.Reason = "linode-pat", "rotate:linode-pat", "   "
	if _, err := routeRotation(in); err == nil {
		t.Error("blank reason must be refused")
	}
	in = base
	in.Scope, in.Reason = "nonsense", "x"
	if _, err := routeRotation(in); err == nil {
		t.Error("unknown scope must be refused")
	}
}

func TestRunCIRotationPlanWritesOutputsAndSummary(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	sum := filepath.Join(t.TempDir(), "sum")
	t.Setenv("GITHUB_OUTPUT", out)
	t.Setenv("GITHUB_STEP_SUMMARY", sum)

	err := runCIRotationPlan(rotationInputs{
		Event: "workflow_dispatch", Scope: "lke-admin", Region: "primary",
		Confirm: "rotate:primary", Reason: "incident", Actor: "octocat",
	})
	if err != nil {
		t.Fatalf("rotation-plan: %v", err)
	}
	got, _ := os.ReadFile(out)
	for _, want := range []string{
		"run-lke-admin=true", "run-pat-create=false", "regions=[\"primary\"]",
		"pat-apply=false", "tf-state-revoke-apply=false",
	} {
		if !strings.Contains(string(got), want+"\n") {
			t.Errorf("GITHUB_OUTPUT missing %q:\n%s", want, got)
		}
	}
	if lines := strings.Count(string(got), "\n"); lines != 11 {
		t.Errorf("GITHUB_OUTPUT has %d lines, want all 11 outputs exactly once", lines)
	}
	summary, _ := os.ReadFile(sum)
	for _, want := range []string{"Emergency rotation requested", "`lke-admin`", "@octocat"} {
		if !strings.Contains(string(summary), want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}

	// A schedule run writes outputs but no dispatch summary.
	os.Remove(out)
	os.Remove(sum)
	if err := runCIRotationPlan(rotationInputs{Event: "schedule", Cron: cronDailyRevoke}); err != nil {
		t.Fatalf("schedule plan: %v", err)
	}
	if _, err := os.Stat(sum); !os.IsNotExist(err) {
		t.Error("schedule run must not write the dispatch summary")
	}

	// Routing refusals surface as errors (the step must fail).
	if err := runCIRotationPlan(rotationInputs{Event: "workflow_dispatch", Reason: ""}); err == nil {
		t.Error("blank reason must fail the step")
	}
}
