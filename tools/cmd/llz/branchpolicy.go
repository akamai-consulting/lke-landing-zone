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
	"fmt"
	"os"
	"os/exec"
	"strings"
)

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

	// 1. Fetch (or create) the environment.
	envJSON, err := ghAPIOut("repos/" + repo + "/environments/" + envName)
	if err != nil {
		// Most likely 404 — create it with the branch policy, then re-fetch. Send a
		// JSON body via --input (not `gh api -f`, which sends every value as a
		// STRING, so the booleans arrive as "false"/"true" and GitHub 422s
		// "is not of type boolean"). json.Marshal types them correctly.
		createBody, _ := json.Marshal(map[string]any{
			"deployment_branch_policy": map[string]any{
				"protected_branches":     false,
				"custom_branch_policies": true,
			},
		})
		if err := execArgv([]string{"gh", "api", "-X", "PUT",
			"repos/" + repo + "/environments/" + envName, "--input", "-"}, string(createBody)); err != nil {
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

	// 3. Flip the policy mode to custom_branch_policies via a GET-then-merge PUT
	//    (PUT replaces the whole config, so preserve reviewers/wait_timer/etc.).
	payload, err := json.Marshal(map[string]any{
		"wait_timer":          numOr(envCfg["wait_timer"], 0),
		"prevent_self_review": boolOr(envCfg["prevent_self_review"], false),
		"reviewers":           sliceOr(envCfg["reviewers"]),
		"deployment_branch_policy": map[string]any{
			"protected_branches":     false,
			"custom_branch_policies": true,
		},
	})
	if err != nil {
		return err
	}
	if err := execArgv([]string{"gh", "api", "-X", "PUT",
		"repos/" + repo + "/environments/" + envName, "--input", "-"}, string(payload)); err != nil {
		return fmt.Errorf("set policy mode on %s: %w", envName, err)
	}

	// 4. Add the `main` rule. POST returns 422 if it already exists — tolerate.
	if out, err := exec.Command("gh", "api", "-X", "POST",
		"repos/"+repo+"/environments/"+envName+"/deployment-branch-policies",
		"-f", "name="+branch, "-f", "type=branch").CombinedOutput(); err != nil {
		if strings.Contains(string(out), "already exists") || strings.Contains(string(out), "already been taken") {
			fmt.Fprintf(os.Stderr, "  ✓ %s rule on %s already exists (race-tolerated)\n", branch, envName)
		} else {
			return fmt.Errorf("add %s rule on %s: %s", branch, envName, strings.TrimSpace(string(out)))
		}
	} else {
		fmt.Fprintf(os.Stderr, "  ✓ %s restricted to ref=%s\n", envName, branch)
	}
	return nil
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
