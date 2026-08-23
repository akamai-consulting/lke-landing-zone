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
	body := map[string]any{
		"deployment_branch_policy": map[string]any{
			"protected_branches":     false,
			"custom_branch_policies": true,
		},
	}
	// READ FROM protection_rules[], WHICH IS WHERE GITHUB PUTS THEM. These three
	// branches keyed on top-level envCfg["reviewers"] / ["wait_timer"] /
	// ["prevent_self_review"], and the environments API returns none of those at the
	// top level — they arrive as entries in `protection_rules`, each tagged by
	// `type`. So all three were dead, and the PUT below always went out WITHOUT
	// them: every run of this dropped a paid repo's manually-configured required
	// reviewers and wait timer, which is precisely what the comment above says it
	// exists to prevent.
	//
	// The shapes also differ between reading and writing, which is the other half
	// of why a naive copy would not have worked: GET returns
	// {"type":"User","reviewer":{"id":1,…}} and PUT wants {"type":"User","id":1}.
	if rv := existingReviewers(envCfg); len(rv) > 0 {
		body["reviewers"] = rv
	}
	if wt := existingWaitTimer(envCfg); wt > 0 {
		body["wait_timer"] = wt
	}
	if existingPreventSelfReview(envCfg) {
		body["prevent_self_review"] = true
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if out, err := ghAPIBody("PUT", "repos/"+repo+"/environments/"+envName, payload); err != nil {
		if isPlanLimitErr(out) {
			return ErrUnsupported // env exists; caller warns + continues
		}
		return fmt.Errorf("set policy mode on %s: %s", envName, strings.TrimSpace(out))
	}

	// 4. Add the `main` rule. POST returns 422 if it already exists — tolerate.
	if out, err := ghForge().Run("api", "-X", "POST",
		branchPoliciesPath(repo, envName),
		"-f", "name="+branch, "-f", "type=branch"); err != nil {
		s := string(out)
		switch {
		case strings.Contains(s, "already exists") || strings.Contains(s, "already been taken"):
			fmt.Fprintf(os.Stderr, "  ✓ %s rule on %s already exists (race-tolerated)\n", branch, envName)
		case isPlanLimitErr(s):
			// The mode flip took but the rule cannot be added on this plan. That is
			// the same lockout as the default case below, so undo it too.
			rollbackBranchPolicyMode(repo, envName)
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
			rollbackBranchPolicyMode(repo, envName)
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
// The body rides in --field-style args rather than stdin because the handle's
// contract is an argv it can inspect; `--input -` would hand it a command whose
// payload it cannot see. `gh api --input` is replaced by writing the JSON to a
// temp file, which keeps the payload out of the process table too.
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

// rollbackBranchPolicyMode undoes step 3's custom_branch_policies flip.
//
// `deployment_branch_policy: null` restores "any branch may deploy", which is the
// state the environment was in before this function touched it. Best-effort and
// LOUD on failure: if the rollback itself cannot run, the operator has to know the
// environment is currently unable to deploy at all.
func rollbackBranchPolicyMode(repo, envName string) {
	payload := []byte(`{"deployment_branch_policy":null}`)
	if out, err := ghAPIBody("PUT", "repos/"+repo+"/environments/"+envName, payload); err != nil {
		fmt.Fprintf(os.Stderr, "::error::%s is in custom-branch-policy mode with NO rules and the rollback "+
			"failed (%s). NOTHING CAN DEPLOY to this environment until it is fixed: set its deployment branch "+
			"policy back to \"all branches\" in the repository settings, or re-run this command once the API "+
			"is reachable.\n", envName, strings.TrimSpace(out))
		return
	}
	fmt.Fprintf(os.Stderr, "  ↩ rolled %s back to unrestricted deploys — custom mode with no rules would "+
		"have blocked every deploy to it\n", envName)
}
