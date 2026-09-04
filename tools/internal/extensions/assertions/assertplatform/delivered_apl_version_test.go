package assertplatform

// delivered_apl_version_test.go — A LANE THAT RUNS NOWHERE, AND AN OVERRIDE THAT
// CANNOT BE SET, are the two ways this gate has already been worth nothing.
//
// Both are properties of YAML in another tree, so neither is visible to any test of
// the lane itself — and both actually happened:
//
//   - The lane was first wired into llz-cluster-health.yml, which is
//     `workflow_dispatch`-only with the step behind an input defaulting to false.
//     A gate that runs when a human clicks it and ticks a box is not a gate, and
//     llz-scheduled-checks.yml says so verbatim about that very workflow.
//   - The blocking failure tells the operator to set LLZ_ALLOW_APL_CHART_MAJOR_DRIFT.
//     On managed App Platform that may be their only move, because LINODE owns the
//     deployed version and a major roll is not something an adopter can revert. GitHub
//     repo variables are not auto-exported, so the remedy existed only where a step
//     passes it through — which was one of the two call sites.

import (
	"os"
	"strings"
	"testing"
)

const (
	scheduledChecks = "../../../../../instance-template/.github/workflows/llz-scheduled-checks.yml"
	e2eBootstrap    = "../../../../../instance-template/.github/workflows/llz-bootstrap-openbao.yml"
	overrideEnv     = "LLZ_ALLOW_APL_CHART_MAJOR_DRIFT"
)

func readDelivered(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// THE LANE RUNS ON A CRON. Not "is referenced somewhere" — the workflow that
// invokes it must actually be scheduled, which is the distinction the first
// placement failed.
func TestAplDeployedVersionRunsOnASchedule(t *testing.T) {
	body := readDelivered(t, scheduledChecks)
	if !strings.Contains(body, "llz ci assert-apl-deployed-version") {
		t.Fatal("llz-scheduled-checks.yml no longer runs assert-apl-deployed-version — drift that appears months after a cluster is built has nothing looking at it")
	}

	// The caller stub owns the trigger surface; assert it carries a cron, or the
	// "scheduled" claim is about a workflow only a human starts.
	caller := readDelivered(t, "../../../../../instance-template/.github/workflows/scheduled-checks.yml")
	if !strings.Contains(caller, "schedule:") || !strings.Contains(caller, "cron:") {
		t.Error("scheduled-checks.yml carries no cron, so the lane's only day-2 runner is manual after all")
	}
}

// AND IT MUST NOT DRIFT BACK into the dispatch-only workflow, where it was first
// put. This is not style: that file's step also sits behind an input defaulting to
// false, so a lane placed there is two conditions away from ever running.
func TestAplDeployedVersionIsNotInTheDispatchOnlyWorkflow(t *testing.T) {
	body := readDelivered(t, "../../../../../instance-template/.github/workflows/llz-cluster-health.yml")
	if strings.Contains(body, "apl-deployed-version") {
		t.Error("apl-deployed-version is back in llz-cluster-health.yml, which is workflow_dispatch-only " +
			"with the step behind a default-false input — llz-scheduled-checks.yml explains why that is not a gate")
	}
}

// stepRunning returns the single workflow step whose `run:` invokes cmd — from its
// own `- name:` to the next one — so an assertion about that step cannot be
// satisfied by text somewhere else in a 900-line file.
func stepRunning(t *testing.T, body, cmd string) string {
	t.Helper()
	// ANCHORED ON THE `run:` INVOCATION, not a bare mention. Both these workflows
	// name their commands in the prose above the step that runs them — the first
	// version of this helper matched llz-bootstrap-openbao.yml's "Run `llz ci
	// assert-suite --list` to see the lanes" comment and scoped itself to the
	// PREVIOUS step, which is how a test about scope acquired a scope bug of its own.
	at := strings.Index(body, "run: "+cmd)
	if at < 0 {
		t.Fatalf("no step has a `run:` invoking %q", cmd)
	}
	start := strings.LastIndex(body[:at], "- name:")
	if start < 0 {
		t.Fatalf("could not find the step header before %q", cmd)
	}
	end := strings.Index(body[at:], "- name:")
	if end < 0 {
		return body[start:]
	}
	return body[start : at+end]
}

// EVERY DELIVERED CALL SITE THAT CAN BLOCK MUST EXPORT THE OVERRIDE, AND EXPORT IT
// ON THE STEP THAT RUNS THE LANE.
//
// A remedy named in an error message and unreachable from the process that printed
// it is worse than no remedy: it sends the operator to a variable that does nothing.
//
// SCOPED TO THE STEP: a GitHub `env:` is per-step, so proximity in the file is not
// scope. Grepping the whole workflow would stay green with the block moved anywhere.
func TestOverrideIsReachableWhereverTheLaneCanBlock(t *testing.T) {
	for _, tc := range []struct{ path, cmd string }{
		{scheduledChecks, "llz ci assert-apl-deployed-version"},
		{e2eBootstrap, "llz ci assert-suite"},
	} {
		body := readDelivered(t, tc.path)
		step := stepRunning(t, body, tc.cmd)
		if !strings.Contains(step, overrideEnv) {
			t.Errorf("%s: the step running %q does not export %s, so the override its failure names cannot be set there",
				tc.path, tc.cmd, overrideEnv)
		}
		// Passed through from `vars.`, not hardcoded: a literal would either disarm
		// the gate for everyone or be dead text.
		if !strings.Contains(step, "vars."+overrideEnv) {
			t.Errorf("%s: the step running %q must take %s from a repo variable, so an adopter can set it without editing a digest-locked file",
				tc.path, tc.cmd, overrideEnv)
		}
	}
}
