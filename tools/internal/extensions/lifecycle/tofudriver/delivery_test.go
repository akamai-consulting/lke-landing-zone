package tofudriver

// delivery_test.go — the two places OUTSIDE this package that have to stay in step
// with it: the dev container that installs `--shell-init`, and the playbook that
// tells operators which verbs need --yes. Neither is reachable from a Go test that
// only looks at Go, which is how both come to drift unnoticed.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot walks up to the template repo root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "instance-template")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("instance-template not found above the test directory — these gates compare " +
				"this package against the SHIPPED scaffold, so a move must fail here rather than " +
				"quietly stop checking")
		}
		dir = parent
	}
}

// postCreateCommand pulls the real hook out of the shipped devcontainer.json.
func postCreateCommand(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "instance-template", ".devcontainer", "devcontainer.json"))
	if err != nil {
		t.Fatal(err)
	}
	// devcontainer.json is JSONC; the shipped file carries the reasoning in
	// comments, which encoding/json will not accept.
	stripped := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(string(b), "")
	var doc struct {
		PostCreateCommand string `json:"postCreateCommand"`
	}
	if err := json.Unmarshal([]byte(stripped), &doc); err != nil {
		t.Fatalf("parsing devcontainer.json: %v", err)
	}
	if doc.PostCreateCommand == "" {
		t.Fatal("the shipped devcontainer.json has no postCreateCommand — the shell hook is how " +
			"a bare `tofu` works in the container, and its absence is the regression")
	}
	return doc.PostCreateCommand
}

// TestDevcontainerInstallsTheShellHook runs the SHIPPED postCreateCommand against
// a throwaway HOME and asserts the outcome rather than pattern-matching it.
// Guarding with `[ -e "$rc" ] || continue` means an image shipping no ~/.bashrc
// gets nothing AND SAYS NOTHING — the feature advertised as automatic would be
// absent, discoverable only by a bare `tofu` still failing.
func TestDevcontainerInstallsTheShellHook(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required to run the shipped postCreateCommand")
	}
	cmd := postCreateCommand(t)
	if !strings.Contains(cmd, "llz tofu --shell-init") {
		t.Fatal("the devcontainer no longer installs the shell hook")
	}

	run := func(t *testing.T, home string) {
		t.Helper()
		ws := filepath.Join(home, "ws")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		// The hook opens with `git config --global --add safe.directory <ws>`,
		// which needs a HOME it may write .gitconfig into — the temp HOME is that.
		c := exec.Command("bash", "-c", strings.ReplaceAll(cmd, "${containerWorkspaceFolder}", ws))
		c.Env = append(os.Environ(), "HOME="+home)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("the shipped postCreateCommand failed: %v\n%s", err, out)
		}
	}

	t.Run("an image with no rc file still gets the hook", func(t *testing.T) {
		home := t.TempDir()
		run(t, home)
		b, err := os.ReadFile(filepath.Join(home, ".bashrc"))
		if err != nil {
			t.Fatalf("no ~/.bashrc was created, so the hook is silently absent: %v", err)
		}
		if !strings.Contains(string(b), "llz tofu --shell-init") {
			t.Errorf(".bashrc does not carry the hook:\n%s", b)
		}
	})

	t.Run("an existing rc file is appended to, not clobbered", func(t *testing.T) {
		home := t.TempDir()
		rc := filepath.Join(home, ".bashrc")
		if err := os.WriteFile(rc, []byte("export PRE_EXISTING=1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, home)
		b, _ := os.ReadFile(rc)
		if !strings.Contains(string(b), "PRE_EXISTING") {
			t.Errorf("the operator's own rc content was destroyed:\n%s", b)
		}
		if !strings.Contains(string(b), "llz tofu --shell-init") {
			t.Errorf("the hook was not appended:\n%s", b)
		}
	})

	t.Run("rebuilding does not duplicate the line", func(t *testing.T) {
		home := t.TempDir()
		run(t, home)
		run(t, home)
		run(t, home)
		b, _ := os.ReadFile(filepath.Join(home, ".bashrc"))
		if n := strings.Count(string(b), "llz tofu --shell-init"); n != 1 {
			t.Errorf("the hook appears %d times after three creates:\n%s", n, b)
		}
	})
}

// TestPlaybookNamesEveryMutatingInitFlag ties the prose to the code.
//
// The playbook tells operators which verbs need --yes. It listed `init` among the
// reads, which was true until mutatingFlags made `init -migrate-state` gated —
// and nothing connected the sentence to the map, so the doc went on contradicting
// the binary. An operator who trusts it hits a refusal the page says cannot
// happen.
func TestPlaybookNamesEveryMutatingInitFlag(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "playbooks", "operator-onboarding.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	for flag := range mutatingFlags["init"] {
		if !strings.Contains(doc, flag) {
			t.Errorf("`init %s` needs --yes and the playbook never mentions it, so the page's "+
				"claim that `init` is a read is wrong for this flag", flag)
		}
	}
}
