package main

// branchpolicy.go ports instance-scripts/ci/set-infra-env-branch-policy.sh into
// llz. The wizard used to shell out to that script by relative path — but a
// rendered instance carries no instance-scripts/ tree (it is sourced from a
// template checkout by the reusable workflows), so `llz secrets push` / `llz
// tokens` could never run it in a real instance. Porting it natively (still via
// `gh api`, like every other GitHub op in llz) closes that gap.
//
// WHY IT MATTERS: GitHub resolves an `environment:`'s secrets at job start, before
// any runtime `if:` check. Without a deployment-branch-policy, anyone with write
// access can dispatch a workflow from a feature branch, select infra-<env>, and
// have GitHub inject the OpenBao unseal keys into a job their branch controls. The
// branch policy is the real boundary — it gates secret injection itself.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// errEnvProtectionUnsupported signals the infra-<env> environment was created (so
// secrets can be pushed) but its main-only branch policy could NOT be applied
// because the repo's plan doesn't include environment protection rules — private
// repos need GitHub Pro/Team/Enterprise. Callers treat it as non-fatal and warn
// the operator to lock it by hand. The branch policy is a defense-in-depth
// boundary, not a prerequisite for the cluster to bootstrap.
var errEnvProtectionUnsupported = errors.New("environment branch protection unsupported on this plan")

// lockInfraEnvBranchPolicy restricts the infra-<env> GitHub Environment to
// deployments from `main` only. Idempotent: skips an env that already has a
// custom `main` policy. Respects --dry-run (prints, changes nothing).
func lockInfraEnvBranchPolicy(g globalOpts, repo, env string) error {
	const branch = "main"
	if repo == "" {
		if a, _ := readAnswers("."); a != nil {
			repo = a.InstanceRepo
		}
	}
	if repo == "" {
		return fmt.Errorf("cannot lock branch policy: instance repo unknown (no .copier-answers.yml)")
	}
	envName := "infra-" + env

	fmt.Fprintf(os.Stderr, "→ lock %s/environments/%s to ref=%s only\n", repo, envName, branch)
	if g.dryRun {
		return nil
	}

	// 1. Fetch (or create) the environment. Create it BARE (empty body, no
	//    protection fields) so it exists on any plan that supports environments at
	//    all — the branch policy is layered on next, best-effort. `gh secret set
	//    --env` needs the environment to exist, so this must succeed.
	envJSON, err := ghAPIOut("repos/" + repo + "/environments/" + envName)
	if err != nil {
		if err := execArgv([]string{"gh", "api", "-X", "PUT",
			"repos/" + repo + "/environments/" + envName}, ""); err != nil {
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
	if policyKind(envCfg) == "custom" && hasMainBranchRule(repo, envName, branch) {
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
	if rv := sliceOr(envCfg["reviewers"]); len(rv) > 0 {
		body["reviewers"] = rv
	}
	if wt := numOr(envCfg["wait_timer"], 0); wt > 0 {
		body["wait_timer"] = wt
	}
	if boolOr(envCfg["prevent_self_review"], false) {
		body["prevent_self_review"] = true
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if out, err := ghAPIBody("PUT", "repos/"+repo+"/environments/"+envName, payload); err != nil {
		if isPlanLimitErr(out) {
			return errEnvProtectionUnsupported // env exists; caller warns + continues
		}
		return fmt.Errorf("set policy mode on %s: %s", envName, strings.TrimSpace(out))
	}

	// 4. Add the `main` rule. POST returns 422 if it already exists — tolerate.
	if out, err := exec.Command("gh", "api", "-X", "POST",
		"repos/"+repo+"/environments/"+envName+"/deployment-branch-policies",
		"-f", "name="+branch, "-f", "type=branch").CombinedOutput(); err != nil {
		s := string(out)
		switch {
		case strings.Contains(s, "already exists") || strings.Contains(s, "already been taken"):
			fmt.Fprintf(os.Stderr, "  ✓ %s rule on %s already exists (race-tolerated)\n", branch, envName)
		case isPlanLimitErr(s):
			return errEnvProtectionUnsupported
		default:
			return fmt.Errorf("add %s rule on %s: %s", branch, envName, strings.TrimSpace(s))
		}
	} else {
		fmt.Fprintf(os.Stderr, "  ✓ %s restricted to ref=%s\n", envName, branch)
	}
	return nil
}

// ghAPIBody runs `gh api -X <method> <path> --input -` piping a JSON body, and
// returns combined output (so the caller can classify the error text).
func ghAPIBody(method, path string, body []byte) (string, error) {
	cmd := exec.Command("gh", "api", "-X", method, path, "--input", "-")
	cmd.Stdin = strings.NewReader(string(body))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// isPlanLimitErr reports whether a gh-api error is GitHub refusing an environment
// protection rule the repo's billing plan doesn't include (private repos need a
// paid plan). The message is: "…ensure the billing plan supports the required
// reviewers protection rule."
func isPlanLimitErr(out string) bool {
	l := strings.ToLower(out)
	return strings.Contains(l, "billing plan") ||
		(strings.Contains(l, "protection rule") && strings.Contains(l, "plan"))
}

// warnEnvProtectionUnsupported tells the operator the infra-<env> environment was
// created + seeded but could NOT be locked to `main`, and how to do it by hand
// once the plan allows. Printed at the END of the run so it isn't buried.
func warnEnvProtectionUnsupported(repo, env string) {
	if repo == "" {
		if a, _ := readAnswers("."); a != nil {
			repo = a.InstanceRepo
		}
	}
	envName := "infra-" + env
	fmt.Fprintf(os.Stderr, "\n%s could not restrict %s to deployments from `main`.\n", yellow("⚠ branch protection skipped"), envName)
	fmt.Fprintln(os.Stderr, dim("  This repo's plan doesn't include environment protection rules (private repos need"))
	fmt.Fprintln(os.Stderr, dim("  GitHub Pro/Team/Enterprise). Secrets were pushed, but until the env is locked a"))
	fmt.Fprintln(os.Stderr, dim("  feature-branch workflow_dispatch could select "+envName+" and read them."))
	fmt.Fprintln(os.Stderr, "  Lock it once the plan allows (UI: Settings → Environments → "+envName+" → Deployment branch policy), or:")
	fmt.Fprintf(os.Stderr, "    %s\n", cyan("gh api -X PUT repos/"+repo+"/environments/"+envName+" -F deployment_branch_policy[custom_branch_policies]=true -F deployment_branch_policy[protected_branches]=false"))
	fmt.Fprintf(os.Stderr, "    %s\n", cyan("gh api -X POST repos/"+repo+"/environments/"+envName+"/deployment-branch-policies -f name=main -f type=branch"))
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

// hasMainBranchRule reports whether the env's custom branch policies include a
// rule named `branch`.
func hasMainBranchRule(repo, envName, branch string) bool {
	out, err := ghAPIOut("repos/" + repo + "/environments/" + envName + "/deployment-branch-policies")
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
func ghAPIOut(args ...string) ([]byte, error) {
	return execOutput("gh", append([]string{"api"}, args...)...)
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
