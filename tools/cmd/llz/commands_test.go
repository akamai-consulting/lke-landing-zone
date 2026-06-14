package main

import (
	"reflect"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/forge"
)

func TestCopierCopyArgv(t *testing.T) {
	// --data llz_version mirrors --vcs-ref, so the rendered instance pins to exactly
	// the release it was scaffolded from.
	got := copierCopyArgv("akamai-consulting", "v0.1.0", "my-instance")
	want := []string{"copier", "copy", "--trust", "--vcs-ref", "v0.1.0",
		"--data", "llz_version=v0.1.0",
		"gh:akamai-consulting/lke-landing-zone", "my-instance"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("copierCopyArgv\n got: %v\nwant: %v", got, want)
	}
}

func TestCopierUpdateArgv(t *testing.T) {
	if got := copierUpdateArgv(""); !reflect.DeepEqual(got, []string{"copier", "update", "--trust"}) {
		t.Errorf("no-ref: got %v", got)
	}
	if got := copierUpdateArgv("v0.2.0"); !reflect.DeepEqual(got,
		[]string{"copier", "update", "--trust", "--vcs-ref", "v0.2.0", "--data", "llz_version=v0.2.0"}) {
		t.Errorf("ref: got %v", got)
	}
}

func TestResolveScaffoldRef(t *testing.T) {
	// Explicit ref is taken verbatim (tag, branch, or SHA).
	if got := resolveScaffoldRef("v0.3.0"); got != "v0.3.0" {
		t.Errorf("explicit ref = %q, want v0.3.0", got)
	}
	if got := resolveScaffoldRef("some-branch"); got != "some-branch" {
		t.Errorf("explicit branch = %q, want some-branch", got)
	}
	// Empty ref falls back to the binary version. In tests `version` is "dev"
	// (not semver), so it resolves to main.
	if got := resolveScaffoldRef(""); got != "main" {
		t.Errorf("dev-build default = %q, want main", got)
	}
}

func TestBootstrapWorkflow(t *testing.T) {
	wf, err := bootstrapWorkflow("dns")
	if err != nil || wf != "bootstrap-dns.yml" {
		t.Fatalf("dns: wf=%q err=%v", wf, err)
	}
	if _, err := bootstrapWorkflow("nope"); err == nil {
		t.Error("expected error for unknown bootstrap kind")
	}
}

// withFakeForge points forgeFn at a fresh Fake for the duration of the test.
func withFakeForge(t *testing.T) *forge.Fake {
	t.Helper()
	fake := forge.NewFake()
	old := forgeFn
	forgeFn = func(string) forge.Forge { return fake }
	t.Cleanup(func() { forgeFn = old })
	return fake
}

func TestCmdBuildRunsWorkflow(t *testing.T) {
	fake := withFakeForge(t)
	if err := cmdBuild([]string{"lab"}, globalOpts{yes: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.Workflows) != 1 || fake.Workflows[0].Workflow != "terraform.yml" {
		t.Fatalf("workflows = %v", fake.Workflows)
	}
	f := fake.Workflows[0].Fields
	if f["region"] != "lab" || f["action"] != "apply" || f["module"] != "all" {
		t.Fatalf("fields = %v", f)
	}
}

func TestCmdBuildDryRunNoExec(t *testing.T) {
	fake := withFakeForge(t)
	if err := cmdBuild([]string{"lab"}, globalOpts{dryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.Workflows) != 0 {
		t.Fatalf("dry-run executed workflow: %v", fake.Workflows)
	}
}

func TestCmdBootstrapRunsWorkflow(t *testing.T) {
	fake := withFakeForge(t)
	if err := cmdBootstrap([]string{"dns", "lab"}, globalOpts{yes: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.Workflows) != 1 || fake.Workflows[0].Workflow != "bootstrap-dns.yml" {
		t.Fatalf("workflows = %v", fake.Workflows)
	}
	if fake.Workflows[0].Fields["region"] != "lab" {
		t.Fatalf("fields = %v", fake.Workflows[0].Fields)
	}
}

func TestValidateEnvName(t *testing.T) {
	// Dynamic deployments: accept any name matching new-deployment.sh's
	// ^[a-z][a-z0-9-]{1,30}$, NOT just a fixed {primary,…,e2e} set. A trailing
	// "-" IS accepted — the contract is exactly that regex.
	valid := []string{"primary", "secondary", "staging", "lab", "e2e", "myteam-dev", "a1", "ab"}
	for _, v := range valid {
		if err := validateEnvName(v); err != nil {
			t.Errorf("validateEnvName(%q) = %v, want nil", v, err)
		}
	}
	invalid := []string{"", "a", "1bad", "Bad", "with_underscore", "has space",
		"way-too-long-environment-name-exceeding-limit"}
	for _, v := range invalid {
		if err := validateEnvName(v); err == nil {
			t.Errorf("validateEnvName(%q) = nil, want error", v)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote([]string{"gh", "secret", "set", "X"}); got != "gh secret set X" {
		t.Errorf("plain: got %q", got)
	}
	if got := shellQuote([]string{"region=us sea"}); got != "'region=us sea'" {
		t.Errorf("space: got %q", got)
	}
}
