package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The routing note is the run log's only statement of WHAT this rotation decided
// to arm. Printing it was untested, so the `plan.Note != ""` guard could invert to
// "print only when there is nothing to say" (a bare newline) unnoticed.
func TestRunCIRotationPlanPrintsTheRoutingNote(t *testing.T) {
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "gha_output"))
	t.Setenv("GITHUB_STEP_SUMMARY", "")

	var err error
	out := captureStdout(t, func() {
		err = runCIRotationPlan(rotationInputs{
			Event: "schedule", Cron: cronMonthlyRotate, Deployments: `["primary"]`,
		})
	})
	if err != nil {
		t.Fatalf("monthly schedule: %v", err)
	}
	for _, want := range []string{"Monthly schedule", "lke-admin", `["primary"]`} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}
