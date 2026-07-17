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
	return forge.NewGitHubSecretWriter(ghAPIBase, token, repo)
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
