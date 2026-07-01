package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
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

func TestRunNewMissingTemplateSource(t *testing.T) {
	// A typo'd / un-forked --org must fail fast with the actionable error instead
	// of letting copier drop into an interactive git username prompt.
	orig := templateSourceExistsFn
	t.Cleanup(func() { templateSourceExistsFn = orig })
	templateSourceExistsFn = func(string) bool { return false }

	err := runNew(globalOpts{}, "nonexistent-org", "v0.1.0", "my-instance", false)
	if err == nil {
		t.Fatal("expected an error when the template source is missing")
	}
	for _, want := range []string{"nonexistent-org/" + templateName, "--org " + defaultTemplateOrg, "gh repo fork"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
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
	// (not semver), so it resolves to "" — the signal for scaffoldRef to look up
	// the latest published release instead of floating on main.
	if got := resolveScaffoldRef(""); got != "" {
		t.Errorf("dev-build sentinel = %q, want \"\"", got)
	}
}

func TestScaffoldRef(t *testing.T) {
	// Explicit ref and the released-binary anchor short-circuit before any lookup.
	if got, err := scaffoldRef("v0.3.0", "org/repo"); err != nil || got != "v0.3.0" {
		t.Errorf("explicit ref = (%q, %v), want (v0.3.0, nil)", got, err)
	}

	orig := latestReleaseFn
	t.Cleanup(func() { latestReleaseFn = orig })

	// Dev build (version=="dev" in tests) → empty sentinel → resolve latest release.
	latestReleaseFn = func(repo string) (string, error) {
		if repo != "org/repo" {
			t.Errorf("latestRelease called with %q, want org/repo", repo)
		}
		return "v9.9.9", nil
	}
	if got, err := scaffoldRef("", "org/repo"); err != nil || got != "v9.9.9" {
		t.Errorf("dev fallback = (%q, %v), want (v9.9.9, nil)", got, err)
	}

	// A resolution failure surfaces an actionable error, never a silent `main`.
	latestReleaseFn = func(string) (string, error) { return "", fmt.Errorf("boom") }
	got, err := scaffoldRef("", "org/repo")
	if err == nil {
		t.Fatalf("expected error on resolution failure, got %q", got)
	}
	if !strings.Contains(err.Error(), "--ref vX.Y.Z") {
		t.Errorf("error %q missing the --ref hint", err)
	}
}

func TestBuildArgv(t *testing.T) {
	got := buildArgv("lab")
	want := []string{"gh", "workflow", "run", "terraform.yml",
		"--field", "region=lab", "--field", "action=apply", "--field", "module=all"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildArgv\n got: %v\nwant: %v", got, want)
	}
}

func TestBootstrapArgv(t *testing.T) {
	dns, err := bootstrapArgv("dns", "lab")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"gh", "workflow", "run", "bootstrap-dns.yml", "--field", "region=lab"}; !reflect.DeepEqual(dns, want) {
		t.Errorf("dns: got %v want %v", dns, want)
	}
	if _, err := bootstrapArgv("nope", "lab"); err == nil {
		t.Error("expected error for unknown bootstrap kind")
	}
}

func TestSecretAndVariableArgv(t *testing.T) {
	// The value must NEVER appear in argv — it is piped via stdin.
	got := secretSetArgv("lab", "LINODE_API_TOKEN")
	want := []string{"gh", "secret", "set", "LINODE_API_TOKEN", "--env", "infra-lab"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("secretSetArgv\n got: %v\nwant: %v", got, want)
	}
	if got := variableSetArgv("TF_STATE_BUCKET"); !reflect.DeepEqual(got,
		[]string{"gh", "variable", "set", "TF_STATE_BUCKET"}) {
		t.Errorf("variableSetArgv: got %v", got)
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
