package branchpolicy

// branchpolicy.go ports instance-scripts/ci/set-infra-env-branch-policy.sh into
// llz. The wizard used to shell out to that script by relative path — but a
// rendered instance carries no instance-scripts/ tree (it is sourced from a
// template checkout by the reusable workflows), so `llz secrets push` / `llz
// tokens` could never run it in a real instance. Porting it natively (still via
// `gh api`, like every other GitHub op in llz) closes that gap.
//
// WHY IT MATTERS: GitHub resolves an `environment:`'s secrets at job start, before
// any runtime `if:` check. Without a Deployment-branch-policy, anyone with write
// access can dispatch a workflow from a feature branch, select infra-<env>, and
// have GitHub inject the OpenBao unseal keys into a job their branch controls. The
// branch policy is the real boundary — it gates secret injection itself.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// ErrUnsupported signals the infra-<env> environment was created (so
// secrets can be pushed) but its main-only branch policy could NOT be applied
// because the repo's plan doesn't include environment protection rules — private
// repos need GitHub Pro/Team/Enterprise. Callers treat it as non-fatal and warn
// the operator to lock it by hand. The branch policy is a defense-in-depth
// boundary, not a prerequisite for the cluster to bootstrap.
var ErrUnsupported = errors.New("environment branch protection unsupported on this plan")

// Lock restricts the infra-<env> GitHub Environment to
// deployments from `main` only. Idempotent: skips an env that already has a
// custom `main` policy. Respects --dry-run (prints, changes nothing).
func Lock(dryRun bool, repo, env string) error {
	const branch = "main"
	if repo == "" {
		repo = instanceRepoFromAnswers()
	}
	if repo == "" {
		return fmt.Errorf("cannot lock branch policy: instance repo unknown (no .copier-answers.yml)")
	}
	envName := "infra-" + env

	fmt.Fprintf(os.Stderr, "→ lock %s/environments/%s to ref=%s only\n", repo, envName, branch)
	if dryRun {
		return nil
	}

	// 1. Fetch (or create) the environment. Create it BARE (empty body, no
	//    protection fields) so it exists on any plan that supports environments at
	//    all — the branch policy is layered on next, best-effort. `gh secret set
	//    --env` needs the environment to exist, so this must succeed.
	// ONLY A DEFINITE 404 MEANS "CREATE IT", and this used to take ANY error as one.
	//
	// The create is a bare `PUT .../environments/<name>` with NO BODY. On an
	// environment that already exists that is not a create — it is a REPLACE, and
	// GitHub reads the omitted wait_timer / reviewers / deployment_branch_policy as
	// null. So a rate limit, a 403, a 5xx or a dropped connection on the GET wiped
	// production's protection rules outright. Worse, the "preserve the operator's
	// reviewers" body below is then rebuilt from a read taken AFTER the wipe, so
	// the preservation was guaranteed to preserve nothing.
	//
	// The rest of this function is careful about not clobbering what an operator
	// set by hand; that care was reachable only when the GET happened to succeed.
	envJSON, err := ghAPIOut("repos/" + repo + "/environments/" + envName)
	if err != nil {
		if !isNotFoundErr(err) {
			return fmt.Errorf("read environment %s: %w — refusing to CREATE it on an inconclusive read: "+
				"the create is a bodyless PUT, which on an environment that already exists clears its "+
				"required reviewers, wait timer and branch policy", envName, err)
		}
		if _, err := ghForge().Run("api", "-X", "PUT",
			"repos/"+repo+"/environments/"+envName); err != nil {
			return fmt.Errorf("create environment %s: %w", envName, err)
		}
		if envJSON, err = ghAPIOut("repos/" + repo + "/environments/" + envName); err != nil {
			return fmt.Errorf("read environment %s after create: %w", envName, err)
		}
	}

	var envCfg map[string]any
	if err := json.Unmarshal(envJSON, &envCfg); err != nil {
		return fmt.Errorf("parse environment %s: %w", envName, err)
	}

	// The PRIOR deployment_branch_policy, captured BEFORE step 3 flips it. The
	// rollback needs it: assuming the prior state was "any branch may deploy" turns
	// a failed lock into an UNRESTRICTED environment on an installation that had
	// protected-branch or custom restrictions already — a fail-closed failure made
	// fail-open, on the one boundary that keeps the OpenBao unseal keys off a
	// branch. Present-but-nil and absent are both "no policy", and both marshal to
	// the `null` GitHub wants, so one value covers all three cases.
	priorPolicy := envCfg["deployment_branch_policy"]

	// 2. Already locked to a custom `main` policy? Skip.
	if policyKind(envCfg) == "custom" && HasMainBranchRule(repo, envName, branch) {
		fmt.Fprintf(os.Stderr, "  ✓ %s already restricted to %s — skipping\n", envName, branch)
		return nil
	}

	// 3. Enable custom branch policies. Send ONLY deployment_branch_policy plus any
	//    EXISTING reviewer/wait-timer protections — never EMPTY ones. Sending an
	//    empty reviewers/wait_timer makes GitHub validate the "required reviewers"
	//    protection rule, which 422s on private repos without a paid plan; including
	//    only already-set values both avoids that and preserves a paid repo's
	//    manually-configured reviewers across the policy flip.
	body := envUpdateBody(envCfg, map[string]any{
		"protected_branches":     false,
		"custom_branch_policies": true,
	})
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if out, err := ghAPIBody("PUT", "repos/"+repo+"/environments/"+envName, payload); err != nil {
		detail := ghDetail(out, err)
		if isPlanLimitErr(detail) {
			return ErrUnsupported // env exists; caller warns + continues
		}
		return fmt.Errorf("set policy mode on %s: %s", envName, detail)
	}

	// 4. Add the `main` rule. POST returns 422 if it already exists — tolerate.
	if out, err := ghForge().Run("api", "-X", "POST",
		branchPoliciesPath(repo, envName),
		"-f", "name="+branch, "-f", "type=branch"); err != nil {
		// ghDetail, not string(out): Forge.Run returns stdout only and `gh api`
		// writes its error bodies to stderr, which kubectlprobe.Exec folds into the
		// error. Classifying on stdout alone sent every transport-level failure to
		// the `default` arm below and printed no reason with it.
		s := ghDetail(string(out), err)
		switch {
		case strings.Contains(s, "already exists") || strings.Contains(s, "already been taken"):
			fmt.Fprintf(os.Stderr, "  ✓ %s rule on %s already exists (race-tolerated)\n", branch, envName)
		case isPlanLimitErr(s):
			// The mode flip took but the rule cannot be added on this plan. That is
			// the same lockout as the default case below, so undo it too.
			rollbackBranchPolicyMode(envCfg, repo, envName, priorPolicy)
			return ErrUnsupported
		default:
			// CUSTOM MODE WITH ZERO RULES BLOCKS EVERY DEPLOY TO THIS ENVIRONMENT.
			// Step 3 above already set custom_branch_policies:true; if this POST
			// fails, the environment is left saying "only the branches in my list
			// may deploy" with an EMPTY list. Nothing could deploy to
			// infra-<deployment> again — no rollback, no self-heal on re-run (step 2
			// skips only when the main rule EXISTS), and no message saying the
			// environment had just been locked out.
			//
			// Undoing the mode flip is strictly better than leaving it: the failure
			// mode of not-applied is "the branch policy is missing", which the next
			// run repairs and which llz warns about. The failure mode of
			// applied-empty is a dead environment.
			// The rollback's OWN outcome decides what this error says. Claiming
			// "has been rolled back" unconditionally, when the rollback PUT may
			// itself have failed, tells the operator the environment is fine at
			// exactly the moment it is locked out — the one message they act on.
			if rerr := rollbackBranchPolicyMode(envCfg, repo, envName, priorPolicy); rerr != nil {
				return fmt.Errorf("add %s rule on %s: %s — AND THE ROLLBACK FAILED (%v). %s is in "+
					"custom-branch-policy mode with NO rules, which blocks EVERY deploy to it. Fix it by "+
					"hand before anything else",
					branch, envName, strings.TrimSpace(s), rerr, envName)
			}
			return fmt.Errorf("add %s rule on %s: %s — the custom-branch-policy mode set a moment ago has "+
				"been rolled back, because an environment in custom mode with no rules blocks EVERY deploy to it",
				branch, envName, strings.TrimSpace(s))
		}
	} else {
		fmt.Fprintf(os.Stderr, "  ✓ %s restricted to ref=%s\n", envName, branch)
	}
	return nil
}

// ghAPIBody runs `gh api -X <method> <path>` with a JSON body, and returns
// combined output (so the caller can classify the error text).
//
// THROUGH THE DECLARED HANDLE, which it was not. This file's own header argues
// that `cloud-mutate` on the binding is what authorises these writes, and every
// mutating call — this PUT, the create in Lock, and the rule POST — went out via
// a bare exec.Command instead. Only the READ used capability.For(...).Forge, so
// removing CloudMutate from the declaration would have changed nothing an
// operator could observe: the fence was documentation.
//
// The body goes in a TEMP FILE named by `--input`, not on stdin. The handle
// classifies an argv, and neither form shows it the payload — `--input -` and
// `--input /tmp/x.json` are equally opaque to it, and pretending otherwise would
// be claiming a fence this does not have. What the file buys is real but
// narrower: the JSON stays out of the process table, where `--field` args would
// have put it, and the argv stays a fixed shape the handle can classify without
// having to parse a body.
func ghAPIBody(method, path string, body []byte) (string, error) {
	f, err := os.CreateTemp("", "llz-branchpolicy-*.json")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	out, err := ghForge().Run("api", "-X", method, path, "--input", f.Name())
	return string(out), err
}

// ghForge is the single place this file reaches the forge, so the binding's
// grants govern every call rather than only the reads.
func ghForge() capability.Forge { return capability.For(policyBinding()).Forge }

// isPlanLimitErr reports whether a gh-api error is GitHub refusing an environment
// protection rule the repo's billing plan doesn't include (private repos need a
// paid plan). The message is: "…ensure the billing plan supports the required
// reviewers protection rule."
func isPlanLimitErr(out string) bool {
	l := strings.ToLower(out)
	return strings.Contains(l, "billing plan") ||
		(strings.Contains(l, "protection rule") && strings.Contains(l, "plan"))
}

// WarnUnsupported tells the operator the infra-<env> environment was
// created + seeded but could NOT be locked to `main`, and how to do it by hand
// once the plan allows. Printed at the END of the run so it isn't buried.
func WarnUnsupported(repo, env string) {
	if repo == "" {
		repo = instanceRepoFromAnswers()
	}
	envName := "infra-" + env
	fmt.Fprintf(os.Stderr, "\n%s could not restrict %s to deployments from `main`.\n", color.Yellow("⚠ branch protection skipped"), envName)
	fmt.Fprintln(os.Stderr, color.Dim("  This repo's plan doesn't include environment protection rules (private repos need"))
	fmt.Fprintln(os.Stderr, color.Dim("  GitHub Pro/Team/Enterprise). Secrets were pushed, but until the env is locked a"))
	fmt.Fprintln(os.Stderr, color.Dim("  feature-branch workflow_dispatch could select "+envName+" and read them."))
	fmt.Fprintln(os.Stderr, "  Lock it once the plan allows (UI: Settings → Environments → "+envName+" → Deployment branch policy), or:")
	fmt.Fprintf(os.Stderr, "    %s\n", color.Cyan("gh api -X PUT repos/"+repo+"/environments/"+envName+" -F deployment_branch_policy[custom_branch_policies]=true -F deployment_branch_policy[protected_branches]=false"))
	fmt.Fprintf(os.Stderr, "    %s\n", color.Cyan("gh api -X POST "+branchPoliciesPath(repo, envName)+" -f name=main -f type=branch"))
}

// policyKind classifies the deployment_branch_policy of an environment config.
func policyKind(envCfg map[string]any) string {
	p, ok := envCfg["deployment_branch_policy"].(map[string]any)
	if !ok || p == nil {
		return "none"
	}
	if b, _ := p["custom_branch_policies"].(bool); b {
		return "custom"
	}
	if b, _ := p["protected_branches"].(bool); b {
		return "protected"
	}
	return "none"
}

// branchPoliciesPath is the deployment-branch-policies collection for an
// environment.
//
// LOWERCASE, AND THAT IS THE WHOLE POINT. GitHub's REST paths are CASE-SENSITIVE:
// this segment was spelled `Deployment-branch-policies` at all three call sites,
// which is not a route GitHub serves, so every call answered 404 Not Found. The
// 404 then read as "the environment does not exist" — in a run whose preceding
// `gh secret set --env infra-prod` calls had all just succeeded against that very
// environment.
//
// It failed in both directions and one of them was silent. HasMainBranchRule 404s
// too, returns false, and the "already restricted — skipping" short-circuit can
// never fire — so an instance whose rule was already correct still fell through to
// the POST and still failed. And because a 404 carries none of the words
// isPlanLimitErr looks for, it bypassed the ErrUnsupported path that exists to let
// a repo without a paid plan finish with a warning instead of an error.
//
// ONE FUNCTION, THREE CALLERS, so the spelling cannot drift between the read, the
// write, and the command printed to the operator — all three were wrong together,
// which is what a copy per call site buys.
func branchPoliciesPath(repo, envName string) string {
	return "repos/" + repo + "/environments/" + envName + "/deployment-branch-policies"
}

// HasMainBranchRule reports whether the env's custom branch policies include a
// rule named `branch`.
func HasMainBranchRule(repo, envName, branch string) bool {
	out, err := ghAPIOut(branchPoliciesPath(repo, envName))
	if err != nil {
		return false
	}
	var doc struct {
		BranchPolicies []struct {
			Name string `json:"name"`
		} `json:"branch_policies"`
	}
	if json.Unmarshal(out, &doc) != nil {
		return false
	}
	for _, bp := range doc.BranchPolicies {
		if bp.Name == branch {
			return true
		}
	}
	return false
}

// ghAPIOut runs `gh api <args>` and returns stdout + an error on non-zero exit
// (the multi-arg, error-returning sibling of state.go's ghAPI(path) []byte).
// IT GOES THROUGH THE DECLARATION NOW. This ran `gh api` via an unconstrained
// process launcher, so the `cloud-mutate` on the binding below was a claim with
// nothing behind it — the same launcher would have run `gh secret set` just as
// happily. capability.For(policyBinding()).Forge classifies each argv by HTTP
// method: the `-X PUT` that writes a deployment-branch-policy is permitted because
// the binding declares cloud-mutate, and a `gh secret set` from here would be
// refused because it does not declare secret-custody.
func ghAPIOut(args ...string) ([]byte, error) {
	return ghForge().Run(append([]string{"api"}, args...)...)
}

func numOr(v any, def float64) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return def
}

func boolOr(v any, def bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

func sliceOr(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return []any{}
}

// instanceRepoFromAnswers reads `instance_repo` from .copier-answers.yml, or ""
// when absent.
func instanceRepoFromAnswers() string {
	a, _ := answers.Read(".")
	if a == nil {
		return ""
	}
	return a.InstanceRepo
}

// ── the environment API's actual response shape ─────────────────────────────
//
// GitHub returns an environment's protections as a TAGGED LIST, not as top-level
// keys:
//
//	{"protection_rules":[
//	   {"id":1,"type":"required_reviewers","prevent_self_review":true,
//	    "reviewers":[{"type":"User","reviewer":{"id":42,…}}]},
//	   {"id":2,"type":"wait_timer","wait_timer":30},
//	   {"id":3,"type":"branch_policy"}],
//	 "deployment_branch_policy":{…}}
//
// The PUT that writes them back takes top-level keys and a DIFFERENT reviewer
// shape ({"type":"User","id":42}), which is why these are three small readers
// rather than a copy of the sub-document.

// protectionRule finds the entry of a given type, or nil.
func protectionRule(envCfg map[string]any, typ string) map[string]any {
	rules, _ := envCfg["protection_rules"].([]any)
	for _, r := range rules {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == typ {
			return m
		}
	}
	return nil
}

// existingReviewers maps the GET's reviewer shape to the PUT's. A reviewer whose
// id cannot be read is DROPPED rather than sent malformed: a 422 on the whole PUT
// would leave the branch policy unapplied, which is worse than losing one entry
// that the operator can re-add.
func existingReviewers(envCfg map[string]any) []any {
	rule := protectionRule(envCfg, "required_reviewers")
	if rule == nil {
		return nil
	}
	raw, _ := rule["reviewers"].([]any)
	out := make([]any, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		who, _ := m["reviewer"].(map[string]any)
		id, ok := who["id"].(float64)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		if typ == "" {
			typ = "User"
		}
		out = append(out, map[string]any{"type": typ, "id": int64(id)})
	}
	return out
}

// existingWaitTimer reads the wait_timer rule's minutes.
func existingWaitTimer(envCfg map[string]any) float64 {
	rule := protectionRule(envCfg, "wait_timer")
	if rule == nil {
		return 0
	}
	return numOr(rule["wait_timer"], 0)
}

// existingPreventSelfReview reads the flag, which GitHub carries ON the
// required_reviewers rule rather than beside it.
func existingPreventSelfReview(envCfg map[string]any) bool {
	rule := protectionRule(envCfg, "required_reviewers")
	if rule == nil {
		return false
	}
	return boolOr(rule["prevent_self_review"], false)
}

// isNotFoundErr reports whether a `gh api` failure was a definite 404 — the only
// error that means "this environment does not exist yet". Everything else is
// inconclusive, and Lock refuses to create on an inconclusive read because the
// create is a bodyless PUT that CLEARS an existing environment's protections.
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "404") || strings.Contains(s, "not found")
}

// rollbackBranchPolicyMode undoes step 3's custom_branch_policies flip by putting
// back the deployment_branch_policy the environment had BEFORE the flip.
//
// IT GOES THROUGH envUpdateBody FOR THE SAME REASON STEP 3 DOES. A PUT to this
// endpoint is a REPLACE: whatever this body omits, GitHub reads as null. A
// rollback body of `{"deployment_branch_policy":null}` alone therefore deletes
// the operator's required reviewers, wait timer and prevent_self_review — the
// exact wipe finding #1 of this review is about, reintroduced in the recovery
// path for it. Sharing the builder is what stops the two from drifting again.
//
// It also restores `prior` rather than hardcoding null. Null means "any branch
// may deploy", which is right only if that is where the environment started; on
// one that already had protected-branch or custom restrictions it strictly
// WEAKENS them, turning a fail-closed failure into a fail-open one.
//
// Best-effort and LOUD: the error is both logged and RETURNED, so the caller's
// message can say which of the two states the operator is actually in.
func rollbackBranchPolicyMode(envCfg map[string]any, repo, envName string, prior any) error {
	payload, err := json.Marshal(envUpdateBody(envCfg, prior))
	if err != nil {
		return err
	}
	if out, err := ghAPIBody("PUT", "repos/"+repo+"/environments/"+envName, payload); err != nil {
		detail := ghDetail(out, err)
		fmt.Fprintf(os.Stderr, "::error::%s is in custom-branch-policy mode with NO rules and the rollback "+
			"failed (%s). NOTHING CAN DEPLOY to this environment until it is fixed: set its deployment branch "+
			"policy back in the repository settings, or re-run this command once the API is reachable.\n",
			envName, detail)
		return fmt.Errorf("rollback %s: %s", envName, detail)
	}
	fmt.Fprintf(os.Stderr, "  ↩ rolled %s back to its previous deployment branch policy — custom mode with "+
		"no rules would have blocked every deploy to it\n", envName)
	return nil
}

// envUpdateBody builds a PUT body for the environments endpoint that carries the
// requested deployment_branch_policy AND everything else the environment already
// had.
//
// THE ENDPOINT IS A REPLACE, NOT A PATCH: every field this body omits, GitHub
// reads as null. So a body naming only the policy silently deletes the
// operator's required reviewers, wait timer and prevent_self_review.
//
// READ FROM protection_rules[], WHICH IS WHERE GITHUB PUTS THEM. The three
// readers below used to key on top-level envCfg["reviewers"] / ["wait_timer"] /
// ["prevent_self_review"], and the environments API returns none of those at the
// top level — they arrive as entries in `protection_rules`, each tagged by
// `type`. So all three were dead and every PUT went out without them, dropping a
// paid repo's manually-configured protections on every single run.
//
// The shapes also differ between reading and writing, which is the other half of
// why a naive copy would not have worked: GET returns
// {"type":"User","reviewer":{"id":1,…}} and PUT wants {"type":"User","id":1}.
//
// Only ALREADY-SET values are included. Sending an empty reviewers/wait_timer
// makes GitHub validate the "required reviewers" protection rule, which 422s on
// a private repo without a paid plan.
func envUpdateBody(envCfg map[string]any, policy any) map[string]any {
	body := map[string]any{"deployment_branch_policy": policy}
	if rv := existingReviewers(envCfg); len(rv) > 0 {
		body["reviewers"] = rv
	}
	if wt := existingWaitTimer(envCfg); wt > 0 {
		body["wait_timer"] = wt
	}
	if existingPreventSelfReview(envCfg) {
		body["prevent_self_review"] = true
	}
	return body
}

// ghDetail is the diagnostic text of a `gh` failure: stdout plus the stderr that
// capability.Forge folds into the error.
//
// Forge.Run returns STDOUT ONLY (kubectlprobe.Exec uses Output(), not
// CombinedOutput), and `gh api` writes its API error bodies to stderr. Every
// caller here that classified on the returned string alone — isPlanLimitErr, the
// "already exists" match, the messages printed to the operator — was reading an
// empty buffer for a transport-level failure, so a 403 or a plan limit reached
// the `default` arm and reported no reason at all.
func ghDetail(out string, err error) string {
	parts := strings.TrimSpace(out)
	if err != nil {
		if e := strings.TrimSpace(err.Error()); e != "" {
			if parts == "" {
				return e
			}
			return parts + ": " + e
		}
	}
	return parts
}
