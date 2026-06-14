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

// GH implements Forge by shelling out to the `gh` CLI — the same mechanism every
// GitHub call in llz used historically. Host pins the forge (empty == github.com;
// set to a GHE host like bits.linode.com to drive GH_HOST and flip the flavor to
// GitHubEnterprise). Repo is the instance "<owner>/<name>" target; it is passed
// via --repo where the subcommand accepts it and woven into REST paths otherwise.
type GH struct {
	Host string
	Repo string
}

// NewGH builds a gh-backed Forge for github.com.
func NewGH(repo string) *GH { return &GH{Repo: repo} }

// NewGHEnterprise builds a gh-backed Forge for a GitHub Enterprise host. The gh
// CLI itself behaves identically (driven by GH_HOST); the distinct flavor lets
// callers select GHE-specific rendered workflow templates.
func NewGHEnterprise(host, repo string) *GH { return &GH{Host: host, Repo: repo} }

var _ Forge = (*GH)(nil)

// Flavor is GitHubEnterprise when a non-github.com host is pinned, else GitHub.
func (g *GH) Flavor() Flavor {
	if g.Host != "" && g.Host != "github.com" {
		return GitHubEnterprise
	}
	return GitHub
}

// env is the process env for gh, adding GH_HOST for non-default (GHE) hosts so
// the CLI targets the instance's forge rather than github.com.
func (g *GH) env() []string {
	e := os.Environ()
	if g.Host != "" && g.Host != "github.com" {
		e = append(e, "GH_HOST="+g.Host)
	}
	return e
}

// repoFlag returns the --repo selector for subcommands that accept it.
func (g *GH) repoFlag() []string {
	if g.Repo != "" {
		return []string{"--repo", g.Repo}
	}
	return nil
}

// capture runs `gh args...` with optional stdin, returning stdout. On failure it
// wraps the forge's stderr (so secret values piped via stdin never leak into the
// error). stdout and stderr are kept separate so callers parsing JSON get clean
// output.
func (g *GH) capture(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
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
		return stdout.Bytes(), fmt.Errorf("gh %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}

func (g *GH) varsPath(s Scope) string {
	if s.Env != "" {
		return "repos/" + g.Repo + "/environments/" + s.Env + "/variables"
	}
	return "repos/" + g.Repo + "/actions/variables"
}

func (g *GH) secretsPath(s Scope) string {
	if s.Env != "" {
		return "repos/" + g.Repo + "/environments/" + s.Env + "/secrets"
	}
	return "repos/" + g.Repo + "/actions/secrets"
}

// SetSecret pipes value into `gh secret set` (stdin keeps it off argv).
func (g *GH) SetSecret(ctx context.Context, name, value string, scope Scope) error {
	args := append([]string{"secret", "set", name}, g.repoFlag()...)
	if scope.Env != "" {
		args = append(args, "--env", scope.Env)
	}
	_, err := g.capture(ctx, []byte(value), args...)
	return err
}

// SetVariable writes an Actions variable (value via stdin; gh reads stdin when
// --body is absent).
func (g *GH) SetVariable(ctx context.Context, name, value string, scope Scope) error {
	args := append([]string{"variable", "set", name}, g.repoFlag()...)
	if scope.Env != "" {
		args = append(args, "--env", scope.Env)
	}
	_, err := g.capture(ctx, []byte(value), args...)
	return err
}

// SecretNames lists configured secret names via the REST API (gh exposes only
// names, never values).
func (g *GH) SecretNames(ctx context.Context, scope Scope) ([]string, error) {
	out, err := g.API(ctx, APIRequest{Path: g.secretsPath(scope)})
	if err != nil {
		return nil, err
	}
	var doc struct {
		Secrets []struct {
			Name string `json:"name"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("parse secrets list: %w", err)
	}
	names := make([]string, 0, len(doc.Secrets))
	for _, s := range doc.Secrets {
		names = append(names, s.Name)
	}
	return names, nil
}

// Variables lists configured variables with their values.
func (g *GH) Variables(ctx context.Context, scope Scope) ([]Variable, error) {
	out, err := g.API(ctx, APIRequest{Path: g.varsPath(scope)})
	if err != nil {
		return nil, err
	}
	var doc struct {
		Variables []Variable `json:"variables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("parse variables list: %w", err)
	}
	return doc.Variables, nil
}

// CreateRepo creates the instance repo from srcDir and pushes it (gh repo
// create; the repo name is positional, so --repo does not apply here).
func (g *GH) CreateRepo(ctx context.Context, srcDir string, private bool) error {
	if g.Repo == "" {
		return fmt.Errorf("CreateRepo: no instance repo configured")
	}
	args := []string{"repo", "create", g.Repo}
	if private {
		args = append(args, "--private")
	}
	args = append(args, "--source", srcDir, "--remote", "origin", "--push")
	_, err := g.capture(ctx, nil, args...)
	return err
}

// API runs `gh api` for endpoints without a typed method yet.
func (g *GH) API(ctx context.Context, req APIRequest) ([]byte, error) {
	args := []string{"api"}
	if req.Method != "" && req.Method != "GET" {
		args = append(args, "-X", req.Method)
	}
	args = append(args, req.Path)
	for k, v := range req.Fields { // gh tolerates any field order
		args = append(args, "-f", k+"="+v)
	}
	var stdin []byte
	if len(req.Body) > 0 {
		args = append(args, "--input", "-")
		stdin = req.Body
	}
	return g.capture(ctx, stdin, args...)
}

// RunWorkflow dispatches a workflow with --field inputs.
func (g *GH) RunWorkflow(ctx context.Context, workflow string, fields map[string]string) error {
	args := append([]string{"workflow", "run", workflow}, g.repoFlag()...)
	for k, v := range fields {
		args = append(args, "--field", k+"="+v)
	}
	_, err := g.capture(ctx, nil, args...)
	return err
}

// LockEnvironmentToBranch restricts the GitHub Environment env to deployments
// from branch only. Idempotent: an env already locked to a custom <branch>
// policy is left untouched. Ported from instance-scripts/ci/set-infra-env-
// branch-policy.sh.
//
// WHY IT MATTERS: GitHub resolves an environment's secrets at job start, before
// any runtime `if:` check. Without a deployment-branch-policy, anyone with write
// access can dispatch a workflow from a feature branch, select the environment,
// and have GitHub inject its secrets into a job their branch controls. The
// branch policy gates secret injection itself.
func (g *GH) LockEnvironmentToBranch(ctx context.Context, env, branch string) error {
	if g.Repo == "" {
		return fmt.Errorf("LockEnvironmentToBranch: no instance repo configured")
	}
	base := "repos/" + g.Repo + "/environments/" + env

	// 1. Fetch (or create) the environment.
	envJSON, err := g.API(ctx, APIRequest{Path: base})
	if err != nil {
		if _, cerr := g.API(ctx, APIRequest{Method: "PUT", Path: base, Fields: map[string]string{
			"deployment_branch_policy[protected_branches]":     "false",
			"deployment_branch_policy[custom_branch_policies]": "true",
		}}); cerr != nil {
			return fmt.Errorf("create environment %s: %w", env, cerr)
		}
		if envJSON, err = g.API(ctx, APIRequest{Path: base}); err != nil {
			return fmt.Errorf("read environment %s after create: %w", env, err)
		}
	}

	var envCfg map[string]any
	if err := json.Unmarshal(envJSON, &envCfg); err != nil {
		return fmt.Errorf("parse environment %s: %w", env, err)
	}

	// 2. Already locked to a custom <branch> policy? Skip.
	if policyKind(envCfg) == "custom" && g.hasBranchRule(ctx, base, branch) {
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
	if _, err := g.API(ctx, APIRequest{Method: "PUT", Path: base, Body: payload}); err != nil {
		return fmt.Errorf("set policy mode on %s: %w", env, err)
	}

	// 4. Add the <branch> rule. POST returns 422 if it already exists — tolerate.
	if _, err := g.API(ctx, APIRequest{Method: "POST", Path: base + "/deployment-branch-policies",
		Fields: map[string]string{"name": branch, "type": "branch"}}); err != nil {
		if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "already been taken") {
			return nil // race-tolerated
		}
		return fmt.Errorf("add %s rule on %s: %w", branch, env, err)
	}
	return nil
}

// hasBranchRule reports whether env's custom branch policies include branch.
func (g *GH) hasBranchRule(ctx context.Context, base, branch string) bool {
	out, err := g.API(ctx, APIRequest{Path: base + "/deployment-branch-policies"})
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
