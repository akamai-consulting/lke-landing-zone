package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/forge"
)

// forge.go is the single point where llz binds to a concrete git forge. Every
// GitHub/GitLab operation in the CLI funnels through forgeFn, so swapping the
// backend (LLZ_FORGE) reroutes the whole tool. The landing zone's OWN github.com
// release downloads (selfupdate.go) deliberately stay outside this — they target
// the template's releases on github.com, not the instance's forge.

// forgeForFn builds the backend for repo, honoring an explicit host override
// (used by `llz openbao regen-root --gh-host`). Selection:
//   - LLZ_FORGE=gitlab        → glab CLI (host: override else LLZ_GITLAB_HOST)
//   - default (github)        → gh CLI; GitHub Enterprise when the resolved host
//     (override else LLZ_GH_HOST) is a non-github.com host.
//
// repo is the instance "<owner>/<name>"; pass "" to let the CLI resolve the repo
// from its ambient auth/context (the pre-migration behavior for bare `gh`).
// It is a var so tests can substitute a *forge.Fake.
var forgeForFn = func(host, repo string) forge.Forge {
	if strings.EqualFold(os.Getenv("LLZ_FORGE"), "gitlab") {
		if host == "" {
			host = os.Getenv("LLZ_GITLAB_HOST")
		}
		return forge.NewGitLab(host, repo)
	}
	if host == "" {
		host = ghHost()
	}
	if host != "" && host != "github.com" {
		return forge.NewGHEnterprise(host, repo)
	}
	return forge.NewGH(repo)
}

// forgeFn is the common test seam: the env-selected backend for repo. Tests
// assign a func returning a *forge.Fake.
var forgeFn = func(repo string) forge.Forge { return forgeForFn("", repo) }

// scopeFor maps an environment label ("" == repo-level) to a forge.Scope.
func scopeFor(env string) forge.Scope {
	if env == "" {
		return forge.RepoLevel
	}
	return forge.Env(env)
}

// gateForge runs fn unless --dry-run / missing --yes, printing desc either way
// (the forge-level analog of runGated, which echoed argv). desc is a
// human-readable summary of the cloud-mutating action.
func gateForge(g globalOpts, desc string, fn func() error) error {
	if g.dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) "+desc)
		return nil
	}
	if !g.yes {
		fmt.Fprintln(os.Stderr, "would: "+desc)
		fmt.Fprintln(os.Stderr, "  (re-run with --yes to execute)")
		return nil
	}
	fmt.Fprintln(os.Stderr, "→ "+desc)
	return fn()
}

func bg() context.Context { return context.Background() }

// forgeWrite is a pending secret/variable write to the forge. It replaces the
// old []string argv items the wizard/tokens flows built for `gh secret set` /
// `gh variable set`.
type forgeWrite struct {
	name   string
	value  string
	secret bool // secret (masked) vs plain variable
	scope  forge.Scope
}

func (w forgeWrite) desc() string {
	kind := "variable"
	if w.secret {
		kind = "secret"
	}
	scope := "repo"
	if w.scope.Env != "" {
		scope = w.scope.Env
	}
	return fmt.Sprintf("set %s %s (%s)", kind, w.name, scope)
}

func (w forgeWrite) apply(ctx context.Context, f forge.Forge) error {
	if w.secret {
		return f.SetSecret(ctx, w.name, w.value, w.scope)
	}
	return f.SetVariable(ctx, w.name, w.value, w.scope)
}

// echoWrites prints the plan (one line per write) to stderr. Secret VALUES are
// never included — only names/scopes — so the printed plan is safe.
func echoWrites(writes []forgeWrite) {
	for _, w := range writes {
		fmt.Fprintln(os.Stderr, "→ "+w.desc())
	}
}

// applyWrites applies writes when --yes and not --dry-run, returning whether it
// executed. Callers echo first (echoWrites) and handle their own follow-ups
// (e.g. the branch-policy lock).
func applyWrites(g globalOpts, f forge.Forge, writes []forgeWrite) (bool, error) {
	if g.dryRun || !g.yes {
		return false, nil
	}
	for _, w := range writes {
		if err := w.apply(bg(), f); err != nil {
			return true, fmt.Errorf("%s: %w", w.name, err)
		}
	}
	return true, nil
}
