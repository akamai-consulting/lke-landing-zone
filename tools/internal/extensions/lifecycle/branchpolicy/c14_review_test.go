package branchpolicy

// c14_review_test.go — the gates for the C14 branchpolicy findings of the
// 2026-08-13 review. This function locks the infra-<env> GitHub Environment to
// ref=main, which is the boundary that stops a pushed branch having the OpenBao
// unseal keys injected into a job it controls. Three of these four could have
// removed or inverted that boundary.

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// ghCall records one `gh` invocation the package made, INCLUDING the JSON body it
// piped. Capturing the payload is what lets these tests assert on what was
// actually SENT rather than on the helper that built it — the difference that let
// two earlier cuts of this file stay green while the call sites were reverted.
type ghCall struct {
	args []string
	body string
}

// stubGH routes every gh call through the package's single exec seam and answers
// from a handler. Routing the MUTATIONS through capability.For(...).Forge is what
// made this possible: they used to bypass it with bare exec.Command, so a test
// could stub the read and the writes still went out for real.
func stubGH(t *testing.T, h func(args []string) ([]byte, error)) *[]ghCall {
	t.Helper()
	var calls []ghCall
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		c := ghCall{args: append([]string(nil), args...)}
		// `gh api --input <file>` — read it now; the caller removes it on return.
		for i, a := range args {
			if a == "--input" && i+1 < len(args) {
				if b, err := os.ReadFile(args[i+1]); err == nil {
					c.body = string(b)
				}
			}
		}
		calls = append(calls, c)
		return h(args)
	})
	return &calls
}

// putBodies returns the JSON payloads of every PUT that carried one, in order.
func putBodies(calls []ghCall) []string {
	var out []string
	for _, c := range calls {
		if c.body != "" && strings.Contains(joined(c), "-X PUT") {
			out = append(out, c.body)
		}
	}
	return out
}

func joined(c ghCall) string { return strings.Join(c.args, " ") }

func sawArgs(calls []ghCall, want ...string) bool {
	for _, c := range calls {
		j := joined(c)
		ok := true
		for _, w := range want {
			if !strings.Contains(j, w) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// ── an inconclusive read must not "create" ──────────────────────────────────

// TestLockRefusesToCreateOnAnInconclusiveRead.
//
// The create is a bare `PUT .../environments/<name>` with NO BODY. On an
// environment that already exists that is not a create — it is a REPLACE, and
// GitHub reads the omitted wait_timer / reviewers / deployment_branch_policy as
// null. So a rate limit, a 403, a 5xx or a dropped connection on the GET wiped
// production's protection rules outright — and the "preserve the operator's
// reviewers" body was then rebuilt from a read taken AFTER the wipe, so the
// preservation was guaranteed to preserve nothing.
func TestLockRefusesToCreateOnAnInconclusiveRead(t *testing.T) {
	for name, msg := range map[string]string{
		"rate limit": "API rate limit exceeded",
		"forbidden":  "HTTP 403: Resource not accessible by integration",
		"server":     "HTTP 502: Bad gateway",
	} {
		t.Run(name, func(t *testing.T) {
			calls := stubGH(t, func([]string) ([]byte, error) { return nil, errors.New(msg) })
			err := Lock(false, "acme/instance", "prod")
			if err == nil {
				t.Fatal("an inconclusive read must not be answered by a bodyless PUT that clears the " +
					"environment's protections")
			}
			if sawArgs(*calls, "-X", "PUT", "environments/infra-prod") {
				t.Error("a PUT went out on an inconclusive read — that is the wipe")
			}
		})
	}
}

// TestLockStillCreatesOnADefiniteNotFound pins the exclusion: a genuinely absent
// environment must still be created, or no instance can ever be scaffolded.
func TestLockStillCreatesOnADefiniteNotFound(t *testing.T) {
	created := false
	calls := stubGH(t, func(args []string) ([]byte, error) {
		j := strings.Join(args, " ")
		switch {
		case strings.Contains(j, "-X PUT") && strings.Contains(j, "environments/infra-prod") && !strings.Contains(j, "--input"):
			created = true
			return []byte("{}"), nil
		case strings.Contains(j, "deployment-branch-policies") && strings.Contains(j, "-X POST"):
			return []byte("{}"), nil
		case strings.Contains(j, "deployment-branch-policies"):
			return []byte(`{"branch_policies":[]}`), nil
		case strings.Contains(j, "--input"):
			return []byte("{}"), nil
		case !created:
			return nil, errors.New("HTTP 404: Not Found")
		}
		return []byte(`{"deployment_branch_policy":null}`), nil
	})
	if err := Lock(false, "acme/instance", "prod"); err != nil {
		t.Fatalf("a definite 404 is the scaffold path and must still create: %v", err)
	}
	if !created {
		t.Error("the environment was never created")
	}
	_ = calls
}

// ── the protections GitHub actually returns ─────────────────────────────────

// TestExistingProtectionsAreReadFromProtectionRules.
//
// These were keyed on top-level envCfg["reviewers"] / ["wait_timer"] /
// ["prevent_self_review"], and the environments API returns NONE of those at the
// top level — they arrive as entries in `protection_rules`, tagged by `type`. So
// all three branches were dead and the PUT always went out without them: every
// run dropped a paid repo's manually-configured required reviewers and wait
// timer, which is exactly what the code's own comment says it exists to prevent.
func TestExistingProtectionsAreReadFromProtectionRules(t *testing.T) {
	const body = `{
	  "protection_rules": [
	    {"id":1,"type":"required_reviewers","prevent_self_review":true,
	     "reviewers":[{"type":"User","reviewer":{"id":42,"login":"ops"}},
	                  {"type":"Team","reviewer":{"id":7,"slug":"sre"}}]},
	    {"id":2,"type":"wait_timer","wait_timer":30},
	    {"id":3,"type":"branch_policy"}
	  ],
	  "deployment_branch_policy": null
	}`
	var envCfg map[string]any
	if err := json.Unmarshal([]byte(body), &envCfg); err != nil {
		t.Fatal(err)
	}

	rv := existingReviewers(envCfg)
	if len(rv) != 2 {
		t.Fatalf("reviewers = %v, want both preserved — dropping them is the paid repo's protection gone", rv)
	}
	// The WRITE shape differs from the read shape, which is the other half of why
	// a naive copy of the sub-document would not have worked.
	first, _ := rv[0].(map[string]any)
	if first["type"] != "User" || first["id"] != int64(42) {
		t.Errorf("reviewer[0] = %v, want the PUT's {type,id} shape", first)
	}
	if got := existingWaitTimer(envCfg); got != 30 {
		t.Errorf("wait_timer = %v, want 30", got)
	}
	if !existingPreventSelfReview(envCfg) {
		t.Error("prevent_self_review lives ON the required_reviewers rule, not beside it")
	}
}

// TestLockSendsTheOperatorsProtectionsBackInThePUT drives the REAL call site.
// Asserting on the three reader helpers proves they parse; it does not prove Lock
// calls them, and a first cut of this file did exactly that and stayed green while
// the call site was reverted to the dead top-level keys.
func TestLockSendsTheOperatorsProtectionsBackInThePUT(t *testing.T) {
	const env = `{
	  "protection_rules": [
	    {"id":1,"type":"required_reviewers","prevent_self_review":true,
	     "reviewers":[{"type":"User","reviewer":{"id":42,"login":"ops"}}]},
	    {"id":2,"type":"wait_timer","wait_timer":30}
	  ],
	  "deployment_branch_policy": null
	}`
	calls := stubGH(t, func(args []string) ([]byte, error) {
		j := strings.Join(args, " ")
		switch {
		case strings.Contains(j, "deployment-branch-policies") && strings.Contains(j, "-X POST"):
			return []byte("{}"), nil
		case strings.Contains(j, "deployment-branch-policies"):
			return []byte(`{"branch_policies":[]}`), nil
		case strings.Contains(j, "--input"):
			return []byte("{}"), nil
		}
		return []byte(env), nil
	})
	if err := Lock(false, "acme/instance", "prod"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	bodies := putBodies(*calls)
	if len(bodies) == 0 {
		t.Fatal("no PUT body was captured — this test is watching the wrong thing")
	}
	got := bodies[0]
	for _, want := range []string{`"reviewers"`, `"id":42`, `"wait_timer":30`, `"prevent_self_review":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("the policy PUT dropped %s — every run of this deletes a paid repo's "+
				"manually-configured protections, which is what the code's own comment says it prevents.\nbody: %s",
				want, got)
		}
	}
}

// TestNoProtectionsMeansNoKeysInTheBody pins the exclusion the original comment
// is about: sending an EMPTY reviewers/wait_timer makes GitHub validate the
// required-reviewers rule, which 422s on a private repo without a paid plan.
func TestNoProtectionsMeansNoKeysInTheBody(t *testing.T) {
	var envCfg map[string]any
	if err := json.Unmarshal([]byte(`{"protection_rules":[]}`), &envCfg); err != nil {
		t.Fatal(err)
	}
	if len(existingReviewers(envCfg)) != 0 || existingWaitTimer(envCfg) != 0 || existingPreventSelfReview(envCfg) {
		t.Error("an environment with no protections must contribute no keys to the PUT")
	}
}

// ── never leave the environment locked out ──────────────────────────────────

// TestLockRollsBackWhenTheRuleCannotBeAdded.
//
// Step 3 sets custom_branch_policies:true; step 4 adds the `main` rule. If step 4
// fails, the environment is left saying "only the branches in my list may deploy"
// with an EMPTY list. NOTHING could deploy to infra-<deployment> again — no
// rollback, no self-heal on re-run (the skip at step 2 fires only when the main
// rule EXISTS), and no message saying the environment had just been locked out.
func TestLockRollsBackWhenTheRuleCannotBeAdded(t *testing.T) {
	calls := stubGH(t, func(args []string) ([]byte, error) {
		j := strings.Join(args, " ")
		switch {
		case strings.Contains(j, "deployment-branch-policies") && strings.Contains(j, "-X POST"):
			return []byte("HTTP 500: Internal Server Error"), errors.New("exit 1")
		case strings.Contains(j, "deployment-branch-policies"):
			return []byte(`{"branch_policies":[]}`), nil
		case strings.Contains(j, "--input"):
			return []byte("{}"), nil
		}
		return []byte(`{"protection_rules":[],"deployment_branch_policy":null}`), nil
	})

	err := Lock(false, "acme/instance", "prod")
	if err == nil {
		t.Fatal("a failed rule POST must be reported")
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("the error must say the mode was rolled back, got: %v", err)
	}
	// ASSERTED ON THE PAYLOAD, not on "a PUT happened": the policy-mode PUT is also
	// a PUT with a body, so matching the argv alone cannot tell the two apart — and
	// an earlier cut of this test could not, which let the rollback be removed while
	// staying green. `deployment_branch_policy:null` is the only way back to "any
	// branch may deploy".
	var rolledBack bool
	for _, b := range putBodies(*calls) {
		if strings.Contains(b, `"deployment_branch_policy":null`) {
			rolledBack = true
		}
	}
	if !rolledBack {
		t.Error("no rollback PUT was issued — the environment is left in custom mode with zero rules, " +
			"which blocks EVERY deploy to it, with no self-heal on re-run")
	}
}

// ── the fence the declaration claims ────────────────────────────────────────

// TestEveryGhCallGoesThroughTheDeclaredHandle. This file's header argues that
// `cloud-mutate` on the binding is what authorises these writes — and every
// mutating call went out via a bare exec.Command instead. Only the READ used
// capability.For(...).Forge, so removing CloudMutate from the declaration would
// have changed nothing an operator could observe: the fence was documentation.
//
// The test IS the proof: stubbing the one seam the handle uses now intercepts the
// create, the policy PUT and the rule POST. Before, those three escaped to a real
// `gh` and this test could not have been written.
func TestEveryGhCallGoesThroughTheDeclaredHandle(t *testing.T) {
	calls := stubGH(t, func(args []string) ([]byte, error) {
		j := strings.Join(args, " ")
		switch {
		case strings.Contains(j, "deployment-branch-policies") && strings.Contains(j, "-X POST"):
			return []byte("{}"), nil
		case strings.Contains(j, "deployment-branch-policies"):
			return []byte(`{"branch_policies":[]}`), nil
		case strings.Contains(j, "--input"):
			return []byte("{}"), nil
		}
		return []byte(`{"protection_rules":[],"deployment_branch_policy":null}`), nil
	})
	if err := Lock(false, "acme/instance", "prod"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if !sawArgs(*calls, "-X", "PUT", "--input") {
		t.Error("the policy-mode PUT did not reach the seam — it is bypassing the declared handle")
	}
	if !sawArgs(*calls, "-X", "POST", "deployment-branch-policies") {
		t.Error("the rule POST did not reach the seam — it is bypassing the declared handle")
	}
}

// ── the rollback, which repeated the wipe it exists to recover from ──────────

// lockWithFailingRulePOST drives Lock to the rollback path: the environment
// reads back as `env`, the mode PUT succeeds, and the rule POST fails. When
// rollbackErr is non-nil the SECOND body-carrying PUT — the rollback — fails too.
func lockWithFailingRulePOST(t *testing.T, env string, rollbackErr error) (*[]ghCall, error) {
	t.Helper()
	bodyPUTs := 0
	calls := stubGH(t, func(args []string) ([]byte, error) {
		j := strings.Join(args, " ")
		switch {
		case strings.Contains(j, "deployment-branch-policies") && strings.Contains(j, "-X POST"):
			return nil, errors.New("HTTP 403: Resource not accessible by integration")
		case strings.Contains(j, "deployment-branch-policies"):
			return []byte(`{"branch_policies":[]}`), nil
		case strings.Contains(j, "--input"):
			// Both the mode flip and the rollback are body-carrying PUTs; the
			// rollback is the second.
			bodyPUTs++
			if bodyPUTs >= 2 && rollbackErr != nil {
				return nil, rollbackErr
			}
			return []byte("{}"), nil
		}
		return []byte(env), nil
	})
	return calls, Lock(false, "acme/instance", "prod")
}

// TestTheRollbackDoesNotWipeTheOperatorsProtections.
//
// The rollback PUT carried `{"deployment_branch_policy":null}` and nothing else.
// This endpoint is a REPLACE — every field omitted reads as null — so the
// recovery path deleted the operator's required reviewers, wait timer and
// prevent_self_review: the exact wipe this review's first finding is about,
// reintroduced in the code written to recover from it.
func TestTheRollbackDoesNotWipeTheOperatorsProtections(t *testing.T) {
	const env = `{
	  "protection_rules": [
	    {"id":1,"type":"required_reviewers","prevent_self_review":true,
	     "reviewers":[{"type":"User","reviewer":{"id":42,"login":"ops"}}]},
	    {"id":2,"type":"wait_timer","wait_timer":30}
	  ],
	  "deployment_branch_policy": null
	}`
	calls, err := lockWithFailingRulePOST(t, env, nil)
	if err == nil {
		t.Fatal("a failed rule POST must be reported")
	}
	bodies := putBodies(*calls)
	if len(bodies) < 2 {
		t.Fatalf("want a mode PUT and a rollback PUT, got %d: %v", len(bodies), bodies)
	}
	rollback := bodies[len(bodies)-1]
	for _, want := range []string{`"id":42`, `"wait_timer":30`, `"prevent_self_review":true`} {
		t.Run(want, func(t *testing.T) {
			if !strings.Contains(rollback, want) {
				t.Errorf("the rollback dropped %s — recovering from a lockout by deleting the "+
					"operator's protections trades one incident for another.\nbody: %s", want, rollback)
			}
		})
	}
}

// TestTheRollbackRestoresThePREVIOUSPolicyNotNull.
//
// `deployment_branch_policy: null` means "any branch may deploy". Hardcoding it
// is right only if that is where the environment started; on one that already
// restricted deploys to protected branches it strictly WEAKENS the setting —
// turning a fail-closed failure into a fail-open one, on the single boundary
// that keeps the OpenBao unseal keys off a pushed branch.
func TestTheRollbackRestoresThePREVIOUSPolicyNotNull(t *testing.T) {
	const env = `{
	  "protection_rules": [],
	  "deployment_branch_policy": {"protected_branches": true, "custom_branch_policies": false}
	}`
	calls, err := lockWithFailingRulePOST(t, env, nil)
	if err == nil {
		t.Fatal("a failed rule POST must be reported")
	}
	bodies := putBodies(*calls)
	if len(bodies) < 2 {
		t.Fatalf("want a mode PUT and a rollback PUT, got %d: %v", len(bodies), bodies)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(bodies[len(bodies)-1]), &got); err != nil {
		t.Fatal(err)
	}
	p, ok := got["deployment_branch_policy"].(map[string]any)
	if !ok {
		t.Fatalf("the rollback set deployment_branch_policy to %v — the environment restricted deploys "+
			"to protected branches before this ran, and it is now UNRESTRICTED", got["deployment_branch_policy"])
	}
	if b, _ := p["protected_branches"].(bool); !b {
		t.Errorf("protected_branches was not restored: %v", p)
	}
}

// TestAFailedRollbackIsNotReportedAsARolledBackEnvironment. The error said "has
// been rolled back" unconditionally, and the rollback returned nothing — so on
// the one path where the environment really is locked out, the operator was told
// it had been repaired.
func TestAFailedRollbackIsNotReportedAsARolledBackEnvironment(t *testing.T) {
	const env = `{"protection_rules":[],"deployment_branch_policy":null}`
	_, err := lockWithFailingRulePOST(t, env, errors.New("HTTP 502: Bad gateway"))
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "has been rolled back") {
		t.Errorf("the rollback itself failed and the environment is locked out, but the error claims "+
			"it was repaired: %v", err)
	}
	if !strings.Contains(err.Error(), "ROLLBACK FAILED") {
		t.Errorf("the message must name the state the operator is actually in: %v", err)
	}
}

// TestAFailureReasonSurvivesTheForgeHandle. capability.Forge.Run returns STDOUT
// ONLY (kubectlprobe.Exec uses Output(), not CombinedOutput) and `gh api` writes
// its API error bodies to stderr. Classifying on the returned string alone meant
// a plan-limit 422 on the rule POST read as an unclassified failure and reported
// no reason at all.
func TestAFailureReasonSurvivesTheForgeHandle(t *testing.T) {
	const env = `{"protection_rules":[],"deployment_branch_policy":null}`
	calls := stubGH(t, func(args []string) ([]byte, error) {
		j := strings.Join(args, " ")
		switch {
		case strings.Contains(j, "deployment-branch-policies") && strings.Contains(j, "-X POST"):
			// stdout EMPTY, reason in the error — what Forge.Run really returns.
			return nil, errors.New("HTTP 422: Deployment protection rules are not available for private repositories on this billing plan")
		case strings.Contains(j, "deployment-branch-policies"):
			return []byte(`{"branch_policies":[]}`), nil
		case strings.Contains(j, "--input"):
			return []byte("{}"), nil
		}
		return []byte(env), nil
	})
	err := Lock(false, "acme/instance", "prod")
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("a plan-limit failure whose text is on stderr must still classify as unsupported, got %v", err)
	}
	if !sawArgs(*calls, "-X", "PUT", "--input") {
		t.Error("fixture drift: no PUT with a body was made")
	}
}
