// Package forge abstracts the git-forge operations llz performs against an
// INSTANCE repo (e.g. lke-landing-zone-example). An instance may live on
// github.com, on GitHub Enterprise (LLZ_GH_HOST), or on GitLab (LLZ_FORGE), so
// these operations sit behind an interface. Three backends implement it today:
// GH (gh CLI, github.com + GHE) and GL (glab CLI, GitLab).
//
// DELIBERATELY OUT OF SCOPE: the landing zone's OWN github.com presence — the
// `llz` self-update release downloads and the landing zone's build/test/release
// CI. Those are always github.com and stay as concrete calls in package main.
// The boundary modeled here is exactly the set of operations parameterized by a
// host + instance repo.
package forge

import (
	"context"
	"errors"
)

// ErrUnsupported is returned by operations a given backend cannot serve (e.g.
// the GitHub-shaped REST escape hatch on a GitLab backend).
var ErrUnsupported = errors.New("operation not supported by this forge")

// Flavor identifies the concrete forge a backend talks to. Callers use it to
// gate forge-specific behavior (e.g. GitHub-Actions attestation only runs on a
// GitHub flavor; GHE selects different rendered workflow templates).
type Flavor string

const (
	GitHub           Flavor = "github"
	GitHubEnterprise Flavor = "github-enterprise"
	GitLab           Flavor = "gitlab"
)

// Scope locates a secret/variable on the forge. The zero value is repo-level;
// a non-empty Env is the deployment Environment (GitHub) / environment-scoped
// CI variable (GitLab), e.g. "infra-dev".
type Scope struct {
	Env string
}

// RepoLevel is the repo-scoped Scope, spelled out for call-site clarity.
var RepoLevel = Scope{}

// Env returns the environment-scoped Scope for name.
func Env(name string) Scope { return Scope{Env: name} }

// Variable is a forge CI variable (value is readable, unlike a secret).
type Variable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// APIRequest is the GitHub-shaped REST escape hatch for endpoints without a
// first-class method — used by the GitHub-Actions attestation scan. Path is the
// full forge-relative API path (e.g. "repos/o/r/environments/infra-dev").
// Non-GitHub backends return ErrUnsupported.
type APIRequest struct {
	Method string            // "", "GET", "PUT", "POST", ... ("" == GET)
	Path   string            // e.g. "repos/o/r/environments/infra-dev"
	Fields map[string]string // -f key=value form fields
	Body   []byte            // raw JSON body (--input -); takes precedence over Fields
}

// Vcs is the source-forge surface against an instance repo: secrets, variables,
// repo creation, environment branch locking, and the REST escape hatch.
type Vcs interface {
	// SetSecret writes a secret (value piped via stdin, never on argv).
	SetSecret(ctx context.Context, name, value string, scope Scope) error
	// SetVariable writes a CI variable.
	SetVariable(ctx context.Context, name, value string, scope Scope) error
	// SecretNames lists configured secret names (values are never readable).
	SecretNames(ctx context.Context, scope Scope) ([]string, error)
	// Variables lists configured variables with their values.
	Variables(ctx context.Context, scope Scope) ([]Variable, error)
	// CreateRepo creates the instance repo from srcDir and pushes it.
	CreateRepo(ctx context.Context, srcDir string, private bool) error
	// LockEnvironmentToBranch restricts the deployment Environment env so it can
	// only be deployed from branch. Idempotent. This is the forge-agnostic
	// replacement for the raw GitHub deployment-branch-policy REST dance.
	LockEnvironmentToBranch(ctx context.Context, env, branch string) error
	// API is the GitHub-shaped REST escape hatch (attestation). Non-GitHub
	// backends return ErrUnsupported. Returns stdout; the error carries stderr.
	API(ctx context.Context, req APIRequest) ([]byte, error)
}

// Runner is the CI/Actions surface against an instance repo.
type Runner interface {
	// RunWorkflow dispatches workflow with field inputs.
	RunWorkflow(ctx context.Context, workflow string, fields map[string]string) error
}

// Forge is the cohesive instance backend: one implementation serves both the
// source and CI surfaces. Call sites should depend on the narrowest slice they
// use (Vcs, Runner) rather than on Forge directly.
type Forge interface {
	Vcs
	Runner
	// Flavor reports which concrete forge this backend talks to.
	Flavor() Flavor
}
