package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// GL implements Forge by shelling out to the `glab` CLI. It is a best-effort
// mapping of llz's GitHub-shaped operations onto GitLab and has NOT been
// exercised against a live GitLab instance (this repo is GitHub-centric).
// Notable model differences, encoded below:
//
//   - GitLab has no secret/variable split: both are "CI/CD variables". We map
//     SetSecret -> a masked variable and SetVariable -> a plain variable, and
//     read them back filtered on the `masked` flag.
//   - GitLab scopes variables by `environment_scope` ("*" == repo-level) rather
//     than by a separate Environment object. Scope.Env maps to that.
//   - GitLab has no per-file workflow_dispatch; a pipeline runs .gitlab-ci.yml.
//     RunWorkflow triggers a pipeline and passes the workflow name + inputs as
//     pipeline variables.
//   - GitLab deployment gating is protected environments + protected branches,
//     not GitHub deployment-branch-policies. LockEnvironmentToBranch maps onto
//     those as closely as the model allows (see its doc).
//
// Host pins a self-managed GitLab (empty == gitlab.com); Repo is the
// "group/project" path.
type GL struct {
	Host string
	Repo string
}

// NewGitLab builds a glab-backed Forge. host "" targets gitlab.com.
func NewGitLab(host, repo string) *GL { return &GL{Host: host, Repo: repo} }

var _ Forge = (*GL)(nil)

func (g *GL) Flavor() Flavor { return GitLab }

func (g *GL) env() []string {
	e := os.Environ()
	if g.Host != "" && g.Host != "gitlab.com" {
		e = append(e, "GITLAB_HOST="+g.Host)
	}
	return e
}

func (g *GL) repoFlag() []string {
	if g.Repo != "" {
		return []string{"--repo", g.Repo}
	}
	return nil
}

// capture runs `glab args...` with optional stdin, returning stdout; the error
// carries stderr (keeping piped secret values out of it).
func (g *GL) capture(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "glab", args...)
	cmd.Env = g.env()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("glab %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}

// scopeArg maps a Scope to glab's --scope flag value ("*" is repo-level).
func scopeArg(s Scope) string {
	if s.Env != "" {
		return s.Env
	}
	return "*"
}

// SetSecret writes a masked CI/CD variable (value piped via stdin).
func (g *GL) SetSecret(ctx context.Context, name, value string, scope Scope) error {
	args := append([]string{"variable", "set", name, "--masked", "--scope", scopeArg(scope)}, g.repoFlag()...)
	_, err := g.capture(ctx, []byte(value), args...)
	return err
}

// SetVariable writes a plain CI/CD variable (value piped via stdin).
func (g *GL) SetVariable(ctx context.Context, name, value string, scope Scope) error {
	args := append([]string{"variable", "set", name, "--scope", scopeArg(scope)}, g.repoFlag()...)
	_, err := g.capture(ctx, []byte(value), args...)
	return err
}

// glVar is the GitLab CI/CD variable shape returned by the API.
type glVar struct {
	Key              string `json:"key"`
	Value            string `json:"value"`
	Masked           bool   `json:"masked"`
	EnvironmentScope string `json:"environment_scope"`
}

// listVariables reads all CI/CD variables for the project via glab api.
func (g *GL) listVariables(ctx context.Context) ([]glVar, error) {
	out, err := g.capture(ctx, nil, "api", "--paginate",
		"projects/"+pathEncode(g.Repo)+"/variables")
	if err != nil {
		return nil, err
	}
	var vars []glVar
	if err := json.Unmarshal(out, &vars); err != nil {
		return nil, fmt.Errorf("parse variables list: %w", err)
	}
	return vars, nil
}

// SecretNames lists masked-variable names in the given scope.
func (g *GL) SecretNames(ctx context.Context, scope Scope) ([]string, error) {
	vars, err := g.listVariables(ctx)
	if err != nil {
		return nil, err
	}
	want := scopeArg(scope)
	var names []string
	for _, v := range vars {
		if v.Masked && v.EnvironmentScope == want {
			names = append(names, v.Key)
		}
	}
	return names, nil
}

// Variables lists plain (non-masked) variables in the given scope.
func (g *GL) Variables(ctx context.Context, scope Scope) ([]Variable, error) {
	vars, err := g.listVariables(ctx)
	if err != nil {
		return nil, err
	}
	want := scopeArg(scope)
	var out []Variable
	for _, v := range vars {
		if !v.Masked && v.EnvironmentScope == want {
			out = append(out, Variable{Name: v.Key, Value: v.Value})
		}
	}
	return out, nil
}

// CreateRepo creates the project and pushes srcDir's default branch.
func (g *GL) CreateRepo(ctx context.Context, srcDir string, private bool) error {
	if g.Repo == "" {
		return fmt.Errorf("CreateRepo: no instance repo configured")
	}
	args := []string{"repo", "create", g.Repo}
	if private {
		args = append(args, "--private")
	}
	// glab has no --source/--push; create the project, then push the tree.
	if _, err := g.capture(ctx, nil, args...); err != nil {
		return err
	}
	// Best-effort: a real instance would wire the remote + push the srcDir tree
	// here. Left explicit rather than silently succeeding (no live GitLab to
	// validate the push flow against).
	return fmt.Errorf("CreateRepo: glab push from %s not yet implemented (project created)", srcDir)
}

// API is the GitHub-shaped escape hatch — unsupported on GitLab (the attestation
// scan that uses it is GitHub-Actions-specific).
func (g *GL) API(_ context.Context, _ APIRequest) ([]byte, error) {
	return nil, ErrUnsupported
}

// RunWorkflow triggers a pipeline, passing the workflow name + inputs as
// pipeline variables (GitLab has no per-file workflow_dispatch).
func (g *GL) RunWorkflow(ctx context.Context, workflow string, fields map[string]string) error {
	args := append([]string{"ci", "run"}, g.repoFlag()...)
	args = append(args, "--variables", "LLZ_WORKFLOW:"+workflow)
	for k, v := range fields {
		args = append(args, "--variables", k+":"+v)
	}
	_, err := g.capture(ctx, nil, args...)
	return err
}

// LockEnvironmentToBranch maps GitHub's deployment-branch-policy onto GitLab's
// model: it protects branch (protected branches gate who can push/deploy from
// it) and ensures a protected environment named env. GitLab does not bind a
// single branch to an environment the way GitHub does, so this is the closest
// available posture rather than an exact equivalent.
func (g *GL) LockEnvironmentToBranch(ctx context.Context, env, branch string) error {
	if g.Repo == "" {
		return fmt.Errorf("LockEnvironmentToBranch: no instance repo configured")
	}
	proj := "projects/" + pathEncode(g.Repo)
	// Protect the branch (idempotent: 409 if already protected — tolerate).
	if _, err := g.capture(ctx, nil, "api", "-X", "POST", proj+"/protected_branches",
		"-f", "name="+branch); err != nil {
		if !strings.Contains(err.Error(), "already protected") && !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("protect branch %s: %w", branch, err)
		}
	}
	// Protect the environment (best-effort; tolerate already-protected).
	if _, err := g.capture(ctx, nil, "api", "-X", "POST", proj+"/protected_environments",
		"-f", "name="+env, "-f", "deploy_access_levels[][access_level]=40"); err != nil {
		if !strings.Contains(err.Error(), "already") {
			return fmt.Errorf("protect environment %s: %w", env, err)
		}
	}
	return nil
}

// pathEncode URL-encodes a "group/project" path for GitLab API :id segments.
func pathEncode(repo string) string {
	return strings.ReplaceAll(repo, "/", "%2F")
}
