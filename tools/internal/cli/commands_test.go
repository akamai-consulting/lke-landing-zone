package cli

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/copier"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envreq"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghcli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/validate"
)

func TestCopierCopyArgv(t *testing.T) {
	// --data llz_version mirrors --vcs-ref, so the rendered instance pins to exactly
	// the release it was scaffolded from.
	got := copier.CopyArgv("akamai-consulting", "v0.0.38", "my-instance")
	want := []string{"copier", "copy", "--trust", "--vcs-ref", "v0.0.38",
		"--data", "llz_version=v0.0.38",
		"gh:akamai-consulting/lke-landing-zone", "my-instance"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("copier.CopyArgv\n got: %v\nwant: %v", got, want)
	}
}

// --defaults is not cosmetic — see copier.UpdateArgv's comment and
// TestCopierUpdateArgvIsNonInteractive. Without it `copier update` re-asks every
// question, which is three silent re-answer prompts by hand and an unhandled
// prompt_toolkit exception with no terminal.
func TestCopierUpdateArgv(t *testing.T) {
	if got := copier.UpdateArgv(""); !reflect.DeepEqual(got, []string{"copier", "update", "--trust", "--defaults"}) {
		t.Errorf("no-ref: got %v", got)
	}
	if got := copier.UpdateArgv("v0.0.39"); !reflect.DeepEqual(got,
		[]string{"copier", "update", "--trust", "--defaults", "--vcs-ref", "v0.0.39", "--data", "llz_version=v0.0.39"}) {
		t.Errorf("ref: got %v", got)
	}
}

func TestResolveScaffoldRef(t *testing.T) {
	// Explicit ref is taken verbatim (tag, branch, or SHA).
	if got := copier.ResolveRef("v0.3.0"); got != "v0.3.0" {
		t.Errorf("explicit ref = %q, want v0.3.0", got)
	}
	if got := copier.ResolveRef("some-branch"); got != "some-branch" {
		t.Errorf("explicit branch = %q, want some-branch", got)
	}
	// Empty ref falls back to the binary version. In tests `version` is "dev"
	// (not llzver.Semver), so it resolves to "" — the signal for copier.Ref to look up
	// the latest published release instead of floating on main.
	if got := copier.ResolveRef(""); got != "" {
		t.Errorf("dev-build sentinel = %q, want \"\"", got)
	}
}

func TestScaffoldRef(t *testing.T) {
	// Explicit ref and the released-binary anchor short-circuit before any lookup.
	if got, err := copier.Ref("v0.3.0", "org/repo"); err != nil || got != "v0.3.0" {
		t.Errorf("explicit ref = (%q, %v), want (v0.3.0, nil)", got, err)
	}

	orig := copier.LatestReleaseFn
	t.Cleanup(func() { copier.LatestReleaseFn = orig })

	// Dev build (version=="dev" in tests) → empty sentinel → resolve latest release.
	copier.LatestReleaseFn = func(repo string) (string, error) {
		if repo != "org/repo" {
			t.Errorf("llzver.LatestRelease called with %q, want org/repo", repo)
		}
		return "v9.9.9", nil
	}
	if got, err := copier.Ref("", "org/repo"); err != nil || got != "v9.9.9" {
		t.Errorf("dev fallback = (%q, %v), want (v9.9.9, nil)", got, err)
	}

	// A resolution failure surfaces an actionable error, never a silent `main`.
	copier.LatestReleaseFn = func(string) (string, error) { return "", fmt.Errorf("boom") }
	got, err := copier.Ref("", "org/repo")
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

func TestSecretAndVariableArgv(t *testing.T) {
	// The value must NEVER appear in argv — it is piped via stdin.
	got := ghcli.SecretSetArgv("lab", "LINODE_API_TOKEN", envreq.SecretIsEnvScoped("LINODE_API_TOKEN"))
	want := []string{"gh", "secret", "set", "LINODE_API_TOKEN", "--env", "infra-lab"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ghcli.SecretSetArgv\n got: %v\nwant: %v", got, want)
	}
	if got := ghcli.VariableSetArgv("TF_STATE_BUCKET"); !reflect.DeepEqual(got,
		[]string{"gh", "variable", "set", "TF_STATE_BUCKET"}) {
		t.Errorf("ghcli.VariableSetArgv: got %v", got)
	}
}

func TestValidateEnvName(t *testing.T) {
	// Dynamic deployments: accept any name matching new-deployment.sh's
	// ^[a-z][a-z0-9-]{1,30}$, NOT just a fixed {primary,…,e2e} set. A trailing
	// "-" IS accepted — the contract is exactly that regex.
	valid := []string{"primary", "secondary", "staging", "lab", "e2e", "myteam-dev", "a1", "ab"}
	for _, v := range valid {
		if err := validate.EnvName(v); err != nil {
			t.Errorf("validate.EnvName(%q) = %v, want nil", v, err)
		}
	}
	invalid := []string{"", "a", "1bad", "Bad", "with_underscore", "has space",
		"way-too-long-environment-name-exceeding-limit"}
	for _, v := range invalid {
		if err := validate.EnvName(v); err == nil {
			t.Errorf("validate.EnvName(%q) = nil, want error", v)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if got := ghcli.Quote([]string{"gh", "secret", "set", "X"}); got != "gh secret set X" {
		t.Errorf("plain: got %q", got)
	}
	if got := ghcli.Quote([]string{"region=us sea"}); got != "'region=us sea'" {
		t.Errorf("space: got %q", got)
	}
}

// ── copier as a prerequisite ─────────────────────────────────────────────────

// withCopierInstalled / withoutCopier stub tool discovery so these tests do not
// depend on whether the machine running them has a Python tool installed.
func withCopierInstalled(t *testing.T) {
	t.Helper()
	withLookPath(t, func(f string) (string, error) { return "/usr/bin/" + f, nil })
}

func withoutCopier(t *testing.T) {
	t.Helper()
	withLookPath(t, func(f string) (string, error) {
		if f == "copier" {
			return "", errors.New(`exec: "copier": executable file not found in $PATH`)
		}
		return "/usr/bin/" + f, nil
	})
}

func TestRequireCopierNamesAnInstallRoute(t *testing.T) {
	// The failure this replaces was exec's own words — `copier copy: exec:
	// "copier": executable file not found in $PATH` — as the second command of the
	// quickstart. Naming the tool is not enough; the operator has never heard of
	// copier, so the error has to carry a way to get it.
	withoutCopier(t)

	err := copier.Require(false, "`llz new`")
	if err == nil {
		t.Fatal("expected a refusal when copier is not on PATH")
	}
	for _, want := range []string{"copier", "llz new", "pipx install copier", "llz doctor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// A --dry-run never execs copier, so it must not hard-fail on its absence — but
// staying silent would report "this would work" about a run that would not.
func TestRequireCopierWarnsButPassesUnderDryRun(t *testing.T) {
	withoutCopier(t)
	var err error
	out := captureStderr(t, func() { err = copier.Require(true, "`llz new`") })
	if err != nil {
		t.Fatalf("--dry-run must not fail on a tool it will not invoke: %v", err)
	}
	for _, want := range []string{"would fail here", "pipx install copier"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run warning missing %q, got: %s", want, out)
		}
	}
}

func TestRequireCopierSilentWhenInstalled(t *testing.T) {
	withCopierInstalled(t)
	if err := copier.Require(false, "`llz new`"); err != nil {
		t.Fatalf("copier is on PATH, nothing to say: %v", err)
	}
}
