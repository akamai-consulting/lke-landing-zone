package newinstance

// new_test.go — the `llz new` tests, moved with their subject out of
// commands_test.go. What stayed behind there is the argv builders and the
// copier.Require cases, which belong to other packages and only shared a file.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/templateid"
)

func TestRunNewMissingTemplateSource(t *testing.T) {
	// A typo'd / un-forked --org must fail fast with the actionable error instead
	// of letting copier drop into an interactive git username onboard.Prompt.
	//
	// copier is stubbed present: runNew now refuses before the GitHub lookup when
	// it is absent, and whether the machine running the tests happens to have a
	// Python tool installed must not decide which error this asserts.
	withCopierInstalled(t)
	withTemplateSourceStatus(t, func(string) (bool, error) { return false, nil })

	err := Run(false, false, "nonexistent-org", "v0.0.38", "my-instance", false)
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

	err := Run(false, false, templateid.DefaultOrg, "v0.1.0", "my-instance", false)
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

func TestRunNewRefusesMissingCopierBeforeCallingGitHub(t *testing.T) {
	// Ordering is the point. copier is what actually renders the scaffold, and the
	// check is free and local — resolving a release tag first would spend two API
	// calls to arrive at the same dead end.
	withoutCopier(t)
	withTemplateSourceStatus(t, func(string) (bool, error) {
		t.Error("GitHub was consulted before the local copier check")
		return true, nil
	})

	err := Run(false, false, templateid.DefaultOrg, "v0.0.40", t.TempDir()+"/my-instance", false)
	if err == nil {
		t.Fatal("expected a refusal when copier is not on PATH")
	}
	if !strings.Contains(err.Error(), "pipx install copier") {
		t.Errorf("error %q is not the copier-missing diagnosis", err)
	}
}
