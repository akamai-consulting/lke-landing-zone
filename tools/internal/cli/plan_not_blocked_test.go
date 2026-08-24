package cli

// plan_not_blocked_test.go — a `tofu plan` must not be blocked by the account's
// capacity or orphan state.
//
// ── WHY ───────────────────────────────────────────────────────────────────────
//
// Every guard in the capacity pre-flight exists to stop an APPLY stalling on the
// account's active-services quota. A plan creates nothing, so there is no quota
// for it to exceed and nothing for it to stall — failing one on the account's
// state punishes a read for the sins of a write.
//
// ── THE COST, MEASURED ────────────────────────────────────────────────────────
//
// release-e2e dispatches a plan straight after its apply to assert the plan is
// empty (`assert_no_changes` → `llz ci assert-upgrade-plan --expect-no-changes`).
// That assertion was blocked here THREE separate times by orphans in a SHARED
// account — 8 live LKE clusters across teams — none of which the run created and
// none of which a plan would touch.
//
// A gate that keeps not running is indistinguishable from one that passes. So a
// blocker on the plan path costs COVERAGE, not merely time, and that is what
// makes this worth pinning rather than leaving to review.
//
// ── WHAT THIS CAN AND CANNOT CHECK ────────────────────────────────────────────
//
// GitHub has no way to splat or introspect an `env:` block, so the mapping is
// hand-maintained YAML and the only way to hold it is to read it — the same
// reason TestDeliveredJobCoversRepoLevelRequirements exists for repo-readiness.
// This asserts the EXPRESSION is plan-aware. It cannot prove GitHub evaluates it
// the way we expect; the e2e lane's own plan dispatch is what proves that.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// blockingKnobs are the three env values that can make the pre-flight FAIL. The
// census-only ones (ORPHAN_THRESHOLD) are deliberately absent: a threshold that
// nothing enforces harms nobody, and requiring it here would be noise.
var blockingKnobs = []string{
	"FAIL_ON_ORPHANS",
	"PREFLIGHT_VPC_LIMIT",
	"PREFLIGHT_VCPU_LIMIT",
}

// preflightStep isolates the capacity pre-flight step's env block.
var preflightStep = regexp.MustCompile(
	`(?s)- name: Pre-flight — Linode account capacity / orphan check\n\s+env:\n(.*?)\n\s+- name: `)

func TestPlanIsNotBlockedByCapacityGuards(t *testing.T) {
	raw, err := os.ReadFile(deliveredPipeline)
	if err != nil {
		t.Fatalf("read %s: %v", deliveredPipeline, err)
	}
	m := preflightStep.FindStringSubmatch(string(raw))
	if m == nil {
		// FAIL CLOSED. If the step was renamed or restructured this test would
		// otherwise pass having read nothing — the vacuous pass this whole gate
		// family exists to refuse.
		t.Fatalf("could not find the capacity pre-flight step in %s — it was renamed or "+
			"restructured, and this gate just examined NOTHING. Re-anchor it.", deliveredPipeline)
	}
	env := m[1]

	for _, knob := range blockingKnobs {
		line := ""
		for _, l := range strings.Split(env, "\n") {
			if strings.Contains(l, knob+":") {
				line = l
				break
			}
		}
		if line == "" {
			t.Errorf("%s is not set in the pre-flight step — either it was removed (then drop it "+
				"from blockingKnobs) or it moved somewhere this gate cannot see it", knob)
			continue
		}
		// Plan-aware means the expression BRANCHES on the dispatched action. Both
		// halves are required: naming inputs.action without 'plan' would branch on
		// destroy, and naming 'plan' without inputs.action cannot branch at all.
		if !strings.Contains(line, "inputs.action") || !strings.Contains(line, "'plan'") {
			t.Errorf("%s can still block a PLAN:\n    %s\n"+
				"    A plan creates nothing, so no capacity or orphan guard applies to it. This blocked\n"+
				"    release-e2e's own `assert_no_changes` dispatch three times on orphans the run did\n"+
				"    not create — and a gate that keeps not running looks exactly like one that passes.\n"+
				"    Neutralise it on the plan path, e.g.\n"+
				"      %s: ${{ (inputs.action == 'plan' && '<inert>') || vars.<VAR> || '<default>' }}",
				knob, strings.TrimSpace(line), knob)
		}
	}
}

// The census must SURVIVE on a plan. Neutralising the guards by skipping the
// whole step would take the forewarning with it — and an operator planning
// before an apply is exactly who wants to hear that the account is full.
func TestThePlanStillGetsTheCensus(t *testing.T) {
	raw, err := os.ReadFile(deliveredPipeline)
	if err != nil {
		t.Fatalf("read %s: %v", deliveredPipeline, err)
	}
	body := string(raw)
	if !strings.Contains(body, "llz ci preflight --deployment") {
		t.Fatal("the pre-flight no longer runs `llz ci preflight` at all")
	}
	// An `if:` that skips the step on a plan would remove the census as well as
	// the block. The step must run and report; only its FAILURE is disarmed.
	if m := preflightStep.FindStringSubmatch(body); m != nil {
		if strings.Contains(m[0], "if: ") && strings.Contains(m[0], "'plan'") {
			t.Error("the pre-flight is SKIPPED on a plan rather than made report-only — " +
				"that removes the account census an operator wants before they apply")
		}
	}
}
