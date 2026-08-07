// Package ghcli is the `gh` command line as llz SHOWS it, not as llz runs it.
//
// internal/ghsecret owns EXECUTION — the writes that actually happen. This owns
// the other half: the argv a dry-run prints, the shell quoting that makes that
// argv copy-pasteable, the host the `gh` CLI is pointed at, and the long "gh is
// probably not installed" diagnostic. Splitting a thing by execute-versus-display
// sounds arbitrary until you notice that Quote has FIVE callers in package main
// that never execute anything — they print a command for a human to run.
//
// EXTRACTED AS AN ENABLER, like ghsecret before harbor and ghgitdata before the
// reconciler. scaffold-instance measured 34 outbound as three files, 25 as nine,
// and ~26 as eleven — it does not converge by adding files, and the residue is
// these five symbols.
package ghcli

import (
	"fmt"
	"os"
	"strings"
)

// SecretSetArgv routes a secret to its scope. Reads the requirement table rather
// than hardcoding --env, matching pushToRepo: TF_STATE_ENCRYPTION_PASSPHRASE is
// repo-level (one per instance), and pushing it env-scoped through `llz secrets
// push` would give a second deployment a different passphrase.
// envScoped IS A PARAMETER RATHER THAN A LOOKUP. It used to call
// configreadiness.SecretIsEnvScoped, which made this substrate package import an
// extension — the inversion the boundary test now forbids. Scoping is a property
// of the requirement table, which is configreadiness's to own; building the argv
// is this package's. The one real caller already imports configreadiness, so
// passing the answer in costs nothing and removes the edge.
func SecretSetArgv(env, name string, envScoped bool) []string {
	argv := []string{"gh", "secret", "set", name}
	if envScoped {
		argv = append(argv, "--env", "infra-"+env)
	}
	return argv
}

func VariableSetArgv(name string) []string {
	return []string{"gh", "variable", "set", name}
}

// Quote renders argv for display, quoting tokens that need it.
func Quote(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		if a == "" || strings.ContainsAny(a, " \t\"'$&|;<>()") {
			b.WriteString("'" + strings.ReplaceAll(a, "'", `'\''`) + "'")
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}

// UnreachableErr covers the case a preflight must NOT blame on the repo: we
// could not ask GitHub at all. `gh` missing or unauthenticated makes every lookup
// fail, and reporting that as "not found" sends the operator off to fix something
// that is fine. tail is the caller's one-line "so do not conclude X" — the checks
// are identical everywhere, the wrong conclusion is not.
// The host is Host(), never the github.com literal: `llz doctor` already scopes
// its own auth check that way (GH_HOST), so a remediation naming the default host
// would tell a GHE operator to authenticate somewhere doctor is not even looking
// — and doctor would go on failing after they followed it exactly.
func UnreachableErr(subject string, err error, tail string) error {
	return fmt.Errorf("could not check %s on GitHub: %w\n"+
		"  llz drives GitHub through the `gh` CLI, so this is almost always gh, not your instance:\n"+
		"  • installed?      gh version                             (https://cli.github.com)\n"+
		"  • authenticated?  gh auth status --hostname %s   (then: gh auth login --hostname %s)\n"+
		"  %s", subject, err, Host(), Host(), tail)
}

// Host is the GitHub host llz authenticates against — github.com unless GH_HOST
// overrides it (e.g. a GHE-hosted template fork). Auth checks scope to this host
// so an unrelated gh account in a broken state doesn't fail the gate.
func Host() string {
	if h := strings.TrimSpace(os.Getenv("GH_HOST")); h != "" {
		return h
	}
	return "github.com"
}
