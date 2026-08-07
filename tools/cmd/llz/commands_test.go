package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghcli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/templateid"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/validate"
)

func TestCopierCopyArgv(t *testing.T) {
	// --data llz_version mirrors --vcs-ref, so the rendered instance pins to exactly
	// the release it was scaffolded from.
	got := copierCopyArgv("akamai-consulting", "v0.0.38", "my-instance")
	want := []string{"copier", "copy", "--trust", "--vcs-ref", "v0.0.38",
		"--data", "llz_version=v0.0.38",
		"gh:akamai-consulting/lke-landing-zone", "my-instance"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("copierCopyArgv\n got: %v\nwant: %v", got, want)
	}
}

func TestRunNewMissingTemplateSource(t *testing.T) {
	// A typo'd / un-forked --org must fail fast with the actionable error instead
	// of letting copier drop into an interactive git username onboard.Prompt.
	//
	// copier is stubbed present: runNew now refuses before the GitHub lookup when
	// it is absent, and whether the machine running the tests happens to have a
	// Python tool installed must not decide which error this asserts.
	withCopierInstalled(t)
	withTemplateSourceStatus(t, func(string) (bool, error) { return false, nil })

	err := runNew(globalOpts{}, "nonexistent-org", "v0.0.38", "my-instance", false)
	if err == nil {
		t.Fatal("expected an error when the template source is missing")
	}
	for _, want := range []string{"nonexistent-org/" + templateid.Name, "--org " + templateid.DefaultOrg, "gh repo fork"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// A `gh` that cannot answer (missing, unauthenticated, offline) must NOT be
// reported as an absent template: the upstream is public, and "fork it first" is
// then the wrong instruction to hand a brand-new adopter.
func TestRunNewGitHubUnreachable(t *testing.T) {
	withCopierInstalled(t)
	withTemplateSourceStatus(t, func(string) (bool, error) {
		return false, errors.New("gh: To get started with GitHub CLI, please run: gh auth login")
	})

	err := runNew(globalOpts{}, templateid.DefaultOrg, "v0.1.0", "my-instance", false)
	if err == nil {
		t.Fatal("expected an error when GitHub could not be reached")
	}
	for _, want := range []string{"could not check", "gh auth status", "NOT a missing template"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "gh repo fork") {
		t.Errorf("told the adopter to fork a template that is fine:\n%s", err)
	}
}

// --defaults is not cosmetic — see copierUpdateArgv's comment and
// TestCopierUpdateArgvIsNonInteractive. Without it `copier update` re-asks every
// question, which is three silent re-answer prompts by hand and an unhandled
// prompt_toolkit exception with no terminal.
func TestCopierUpdateArgv(t *testing.T) {
	if got := copierUpdateArgv(""); !reflect.DeepEqual(got, []string{"copier", "update", "--trust", "--defaults"}) {
		t.Errorf("no-ref: got %v", got)
	}
	if got := copierUpdateArgv("v0.0.39"); !reflect.DeepEqual(got,
		[]string{"copier", "update", "--trust", "--defaults", "--vcs-ref", "v0.0.39", "--data", "llz_version=v0.0.39"}) {
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
	// (not selfupgrade.Semver), so it resolves to "" — the signal for scaffoldRef to look up
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
			t.Errorf("selfupgrade.LatestRelease called with %q, want org/repo", repo)
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

func TestSecretAndVariableArgv(t *testing.T) {
	// The value must NEVER appear in argv — it is piped via stdin.
	got := ghcli.SecretSetArgv("lab", "LINODE_API_TOKEN")
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

func TestCheckNewTarget(t *testing.T) {
	// Absent or empty is the normal case — scaffold away.
	fresh := filepath.Join(t.TempDir(), "does-not-exist-yet")
	if err := checkNewTarget(fresh); err != nil {
		t.Errorf("absent dir: %v", err)
	}
	if err := checkNewTarget(t.TempDir()); err != nil {
		t.Errorf("empty dir: %v", err)
	}

	// An existing INSTANCE. `copier copy` would render a second scaffold on top of
	// it (it prompts per conflicting file, it does not stop), so the retry that
	// looks harmless — re-running `llz new` with the same name — merges a fresh
	// scaffold into a live instance. Name the two commands that were actually meant.
	inst := t.TempDir()
	if err := os.WriteFile(filepath.Join(inst, ".copier-answers.yml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := checkNewTarget(inst)
	if err == nil {
		t.Fatal("expected a refusal to scaffold over an existing instance")
	}
	for _, want := range []string{"already a landing-zone instance", "llz env add", "llz upgrade"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}

	// Non-empty but not an instance: still refuse, without the instance advice.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkNewTarget(other); err == nil {
		t.Error("expected a refusal to scaffold into a non-empty directory")
	}
}

func TestCheckNewTargetIgnoresHiddenEntries(t *testing.T) {
	// Scaffolding into a freshly cloned empty repo (only .git) is a legitimate
	// path, and copier git-inits the dir itself — so hidden entries alone are not
	// "content". A .copier-answers.yml is still caught, by instanceresolve.IsInstanceRoot.
	cloned := t.TempDir()
	if err := os.Mkdir(filepath.Join(cloned, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkNewTarget(cloned); err != nil {
		t.Errorf("a bare .git must not block the scaffold: %v", err)
	}
}

func TestCheckNewTargetUnreadableDirIsNotEmpty(t *testing.T) {
	// A directory that exists but cannot be read is not "absent". Treating every
	// ReadDir error as absence let `llz new` proceed into it, so copier failed
	// later and less legibly — or rendered part of a scaffold first.
	parent := t.TempDir()
	locked := filepath.Join(parent, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skipf("cannot drop read permission here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if os.Geteuid() == 0 {
		t.Skip("running as root — permission bits do not apply")
	}

	err := checkNewTarget(locked)
	if err == nil {
		t.Fatal("an unreadable target must not be treated as an empty one")
	}
	if !strings.Contains(err.Error(), "cannot read") {
		t.Errorf("error %q should name the read failure", err)
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

	err := requireCopier(globalOpts{}, "`llz new`")
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
	out := captureStderr(t, func() { err = requireCopier(globalOpts{dryRun: true}, "`llz new`") })
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
	if err := requireCopier(globalOpts{}, "`llz new`"); err != nil {
		t.Fatalf("copier is on PATH, nothing to say: %v", err)
	}
}

func TestRunNewRefusesMissingCopierBeforeCallingGitHub(t *testing.T) {
	// Ordering is the point. copier is what actually renders the scaffold, and the
	// check is free and local — resolving a release tag first would spend two API
	// calls to arrive at the same dead end.
	withoutCopier(t)
	withTemplateSourceStatus(t, func(string) (bool, error) {
		t.Error("GitHub was consulted before the local copier check")
		return true, nil
	})

	err := runNew(globalOpts{}, templateid.DefaultOrg, "v0.0.40", t.TempDir()+"/my-instance", false)
	if err == nil {
		t.Fatal("expected a refusal when copier is not on PATH")
	}
	if !strings.Contains(err.Error(), "pipx install copier") {
		t.Errorf("error %q is not the copier-missing diagnosis", err)
	}
}
