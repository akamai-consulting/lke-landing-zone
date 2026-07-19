package main

// gh_secrets_native.go — thin, env-sourced adapter over internal/forge's
// GitHubSecretWriter (which holds the sealed-box REST logic). These wrappers
// preserve the signatures the in-cluster callers use — the harbor-robot
// provisioner (repo secrets + existence probe) and the broad-PAT rotator (env
// secret writeback) — while the actual GitHub API work now lives in the forge
// package. See docs/designs/forge-abstraction.md (Phase 2).
//
// Auth/repo come from GH_TOKEN + GH_REPO (owner/name) — the same env contract
// the gh CLI uses, sourced in-cluster from the ESO-synced github-dispatch-token
// Secret + a copier-rendered literal.

import (
	"fmt"
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/forge"
)

// ghAPIBase is the REST API root for these in-cluster native GitHub calls. It is
// resolved from the environment through internal/forge so a non-github.com host
// is honored: GITHUB_API is an explicit override (the same var the CI-side audit
// path in ci_gh_pat_expiry.go already reads — this closes the gap where the write
// path ignored it and would silently target api.github.com on a GHES instance),
// else a non-github.com GH_HOST is treated as a GHES appliance. It stays a
// package var so tests can point it at an httptest server.
var ghAPIBase = resolveGHAPIBase()

func resolveGHAPIBase() string {
	if v := os.Getenv("GITHUB_API"); v != "" {
		return v
	}
	if h := os.Getenv("GH_HOST"); h != "" && h != "github.com" {
		if f, err := forge.New(forge.GHES, h); err == nil {
			return f.APIBase()
		}
	}
	f, _ := forge.New(forge.GitHub, "")
	return f.APIBase()
}

// ghNativeWriter builds a forge SecretWriter from the ambient GH_TOKEN/GH_REPO
// env and the resolved ghAPIBase. Every wrapper below goes through it.
func ghNativeWriter() (*forge.GitHubSecretWriter, error) {
	token, repo := os.Getenv("GH_TOKEN"), os.Getenv("GH_REPO")
	if token == "" || repo == "" {
		return nil, fmt.Errorf("GH_TOKEN and GH_REPO must be set for native GitHub secret writes")
	}
	if err := ghWriteTargetStrictOK(); err != nil {
		return nil, err
	}
	return forge.NewGitHubSecretWriter(ghAPIBase, token, repo)
}

// ghWriteTargetStrictOK is the fail-closed forge-target guard. resolveGHAPIBase
// silently defaults to github.com when no forge host is declared — fine for a
// single-forge instance, but in a multi-lane setup (a github.com e2e lane and a
// GHES lane sharing one Linode account) that default is a clobber vector: a GHES
// lane that forgets GH_HOST/LLZ_FORGE_HOST would write its rotated secret into
// github.com's identically-named infra-<env> environment, overwriting the other
// lane's credential. When LLZ_FORGE_STRICT is set (the e2e/rotation lanes set
// it), an env-scoped write must have an EXPLICIT forge target and aborts rather
// than fall back to github.com. See docs/designs/forge-abstraction.md.
func ghWriteTargetStrictOK() error {
	if os.Getenv("LLZ_FORGE_STRICT") == "" {
		return nil
	}
	for _, k := range []string{"GITHUB_API", "GH_HOST", "LLZ_FORGE_HOST", "LLZ_FORGE"} {
		if os.Getenv(k) != "" {
			return nil
		}
	}
	return fmt.Errorf("LLZ_FORGE_STRICT is set but no forge target is declared " +
		"(GH_HOST / LLZ_FORGE / LLZ_FORGE_HOST / GITHUB_API) — refusing to default to " +
		"github.com and risk clobbering another lane's env-scoped secrets")
}

// ghSetRepoSecretNative writes one repo-level Actions secret.
func ghSetRepoSecretNative(name, value string) error {
	w, err := ghNativeWriter()
	if err != nil {
		return err
	}
	return w.SetRepoSecret(name, value)
}

// ghSetEnvSecretNative writes one environment-scoped Actions secret (the
// infra-<deployment> copies the workflows read).
func ghSetEnvSecretNative(name, env, value string) error {
	w, err := ghNativeWriter()
	if err != nil {
		return err
	}
	return w.SetEnvSecret(env, name, value)
}

// ghRepoSecretExistsNative reports whether a repo-level Actions secret exists.
func ghRepoSecretExistsNative(name string) (bool, error) {
	w, err := ghNativeWriter()
	if err != nil {
		return false, err
	}
	return w.RepoSecretExists(name)
}
