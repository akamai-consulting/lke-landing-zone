// Package ghsecret is the `gh secret` write layer — one place for the four
// helpers that were scattered across four files in package main.
//
// EXTRACTED AHEAD OF ITS CALLER, DELIBERATELY. The OpenBao lifecycle family
// (init, regen-root, breakglass, ensure-ready) has exactly three outbound edges
// that are not localisable noise, and all three are these. Worse, they STRADDLED:
// ci_openbao_init.go DEFINED ghSetSecretFn while ci_baoseed.go and
// credrotate_deps.go consumed it, and ci_clear_secrets.go/commands.go/openbao.go
// went the other way. Moving the lifecycle family without settling this first
// would have dragged a GitHub-secret writer into an OpenBao package and made
// every unrelated caller import OpenBao to set a secret — the same mistake
// appendGHAFile nearly caused one extraction ago.
//
// SEAMS, AND WHICH ONE TO STUB. SetFn and DeleteFn are the vars; Set and Delete
// are the real implementations behind them. Tests stub the Fn vars. Nothing here
// calls the other's seam, so there is no wrapper to accidentally leave live.
package ghsecret

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Set pipes value into `gh secret set <name>` (with --env <ghEnv> when ghEnv is
// non-empty), keeping the value off argv. gh resolves auth + repo from the
// ambient GH_TOKEN/GH_REPO — the same contract the shell scripts ran under.
//
// ghEnv == "" is the REPO-level write, not a missing argument: `harbor`
// writes repo secrets and the OpenBao lanes write environment ones, and one
// function serves both because the only difference is two argv elements.
func Set(name, ghEnv, value string) error {
	args := []string{"secret", "set", name}
	label := name
	if ghEnv != "" {
		args = append(args, "--env", ghEnv)
		label = fmt.Sprintf("%s --env %s", name, ghEnv)
	}
	cmd := exec.Command("gh", args...)
	cmd.Stdin = strings.NewReader(value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh secret set %s: %s", label, strings.TrimSpace(string(out)))
	}
	return nil
}

// Delete removes one env-scoped GitHub secret.
func Delete(name, ghEnv string) error {
	cmd := exec.Command("gh", "secret", "delete", name, "--env", ghEnv)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh secret delete %s --env %s: %s", name, ghEnv, strings.TrimSpace(string(out)))
	}
	return nil
}

// SetFn and DeleteFn are the test seams. They are package vars rather than
// parameters because the call trees that reach them are four and five frames
// deep and every one of those frames would otherwise carry a function argument
// it does not use.
var (
	SetFn    = Set
	DeleteFn = Delete
)

// Mask emits ::add-mask:: for a value when running under GitHub Actions.
//
// It lives beside the writers rather than in a logging package because the thing
// it protects is exactly what they write: every caller masks a value immediately
// before or after storing it as a secret.
func Mask(v string) {
	if os.Getenv("GITHUB_ACTIONS") != "" && v != "" {
		fmt.Printf("::add-mask::%s\n", v)
	}
}
