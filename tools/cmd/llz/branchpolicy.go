package main

// branchpolicy.go locks the infra-<env> deployment Environment to `main` only.
// The forge-specific REST dance (GitHub deployment-branch-policies; GitLab
// protected environments/branches) lives behind forge.LockEnvironmentToBranch;
// this file only resolves the repo and applies the dry-run gate.
//
// WHY IT MATTERS: a forge resolves an environment's secrets at job start, before
// any runtime `if:` check. Without a branch policy, anyone with write access can
// dispatch a workflow from a feature branch, select infra-<env>, and have the
// forge inject the OpenBao unseal keys into a job their branch controls. The
// branch policy is the real boundary — it gates secret injection itself.

import (
	"fmt"
	"os"
)

// lockInfraEnvBranchPolicy restricts the infra-<env> Environment to deployments
// from `main` only. Idempotent (the backend skips an already-locked env).
// Respects --dry-run (prints, changes nothing).
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
	return forgeFn(repo).LockEnvironmentToBranch(bg(), envName, branch)
}
