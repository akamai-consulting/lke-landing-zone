package forge

import (
	"context"
	"errors"
	"testing"
)

func TestFakeSecretsAndVariablesByScope(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	_ = f.SetSecret(ctx, "TOKEN", "s3cr3t", Env("infra-dev"))
	_ = f.SetSecret(ctx, "REPO_TOKEN", "r", RepoLevel)
	_ = f.SetVariable(ctx, "REGION", "us-ord", Env("infra-dev"))

	envSecrets, _ := f.SecretNames(ctx, Env("infra-dev"))
	if len(envSecrets) != 1 || envSecrets[0] != "TOKEN" {
		t.Fatalf("env secrets = %v, want [TOKEN]", envSecrets)
	}
	repoSecrets, _ := f.SecretNames(ctx, RepoLevel)
	if len(repoSecrets) != 1 || repoSecrets[0] != "REPO_TOKEN" {
		t.Fatalf("repo secrets = %v, want [REPO_TOKEN]", repoSecrets)
	}
	vars, _ := f.Variables(ctx, Env("infra-dev"))
	if len(vars) != 1 || vars[0].Value != "us-ord" {
		t.Fatalf("env vars = %v, want one us-ord", vars)
	}
}

func TestFakeRecordsWorkflowAndLock(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	_ = f.RunWorkflow(ctx, "terraform.yml", map[string]string{"region": "dev"})
	_ = f.LockEnvironmentToBranch(ctx, "infra-dev", "main")
	if len(f.Workflows) != 1 || f.Workflows[0].Workflow != "terraform.yml" {
		t.Fatalf("workflows = %v", f.Workflows)
	}
	if len(f.Locks) != 1 || f.Locks[0].Branch != "main" {
		t.Fatalf("locks = %v", f.Locks)
	}
}

func TestGitLabAPIUnsupported(t *testing.T) {
	g := NewGitLab("", "group/proj")
	if _, err := g.API(context.Background(), APIRequest{Path: "x"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("GitLab API err = %v, want ErrUnsupported", err)
	}
}

func TestFlavors(t *testing.T) {
	if got := NewGH("o/r").Flavor(); got != GitHub {
		t.Errorf("NewGH flavor = %q, want github", got)
	}
	if got := NewGHEnterprise("bits.example.com", "o/r").Flavor(); got != GitHubEnterprise {
		t.Errorf("GHE flavor = %q, want github-enterprise", got)
	}
	if got := NewGitLab("", "g/p").Flavor(); got != GitLab {
		t.Errorf("GitLab flavor = %q, want gitlab", got)
	}
}

func TestScopeArgAndPathEncode(t *testing.T) {
	if scopeArg(RepoLevel) != "*" {
		t.Errorf("repo scopeArg = %q, want *", scopeArg(RepoLevel))
	}
	if scopeArg(Env("infra-dev")) != "infra-dev" {
		t.Errorf("env scopeArg = %q", scopeArg(Env("infra-dev")))
	}
	if pathEncode("group/sub/proj") != "group%2Fsub%2Fproj" {
		t.Errorf("pathEncode = %q", pathEncode("group/sub/proj"))
	}
}

func TestPolicyKind(t *testing.T) {
	cases := []struct {
		cfg  map[string]any
		want string
	}{
		{map[string]any{}, "none"},
		{map[string]any{"deployment_branch_policy": nil}, "none"},
		{map[string]any{"deployment_branch_policy": map[string]any{"custom_branch_policies": true}}, "custom"},
		{map[string]any{"deployment_branch_policy": map[string]any{"protected_branches": true}}, "protected"},
		{map[string]any{"deployment_branch_policy": map[string]any{"protected_branches": false}}, "none"},
		{map[string]any{"deployment_branch_policy": map[string]any{"custom_branch_policies": false}}, "none"},
	}
	for _, c := range cases {
		if got := policyKind(c.cfg); got != c.want {
			t.Errorf("policyKind(%v) = %q, want %q", c.cfg, got, c.want)
		}
	}
}

func TestEnvCfgCoercers(t *testing.T) {
	if numOr(float64(7), 0) != 7 || numOr("x", 3) != 3 || numOr(nil, 0) != 0 {
		t.Error("numOr")
	}
	if !boolOr(true, false) || boolOr("x", true) != true || boolOr(nil, false) {
		t.Error("boolOr")
	}
	if len(sliceOr([]any{1, 2})) != 2 || len(sliceOr(nil)) != 0 || len(sliceOr("x")) != 0 {
		t.Error("sliceOr")
	}
}
