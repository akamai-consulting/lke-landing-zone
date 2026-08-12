package upstreamupdates

// cobra_test.go — the command layer, driven through its seams.
//
// The pure judgement is covered in upstreamupdates_test.go. What is covered HERE
// is the wiring around it, which is where the two commands can still go wrong in
// the way that matters: doing something irreversible, or reporting success for
// work they did not do.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runCmd executes a command with args, capturing stderr, and returns the error.
func runCmd(t *testing.T, c *cobra.Command, args []string, errBuf *bytes.Buffer) error {
	t.Helper()
	c.SetArgs(args)
	c.SetErr(errBuf)
	if c.OutOrStdout() == os.Stdout {
		c.SetOut(&bytes.Buffer{})
	}
	return c.Execute()
}

// inTempRepo runs fn with cwd set to an empty temp dir and the Actions output
// files pointed somewhere writable, so a command's ghaout.Append does not append
// to the developer's environment.
func inTempRepo(t *testing.T, fn func(dir string)) {
	t.Helper()
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	t.Setenv("GITHUB_OUTPUT", filepath.Join(dir, "out"))
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(dir, "summary"))
	fn(dir)
}

// writeAnswers seeds the pin the upgrade would have just rewritten — the single
// source `upgrade-pr` reads the version from.
func writeAnswers(t *testing.T, dir, version string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"),
		[]byte("_commit: abc\nllz_version: \""+version+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ── pr-touches ──────────────────────────────────────────────────────────────

func TestPRTouchesWritesTheOutputAndFailsClosedOnAPIError(t *testing.T) {
	orig := listChangedFiles
	t.Cleanup(func() { listChangedFiles = orig })

	t.Run("writes true", func(t *testing.T) {
		listChangedFiles = func(string) ([]string, error) {
			return []string{"terraform-iac-bootstrap/cluster/main.tf"}, nil
		}
		inTempRepo(t, func(dir string) {
			var errBuf bytes.Buffer
			c := PRTouchesCmd()
			c.SetOut(&bytes.Buffer{})
			if err := runCmd(t, c, []string{"--base-sha", "abc123",
				"--prefix", "terraform-iac-bootstrap/", "--output-name", "terraform"}, &errBuf); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			out, _ := os.ReadFile(filepath.Join(dir, "out"))
			if !strings.Contains(string(out), "terraform=true") {
				t.Errorf("GITHUB_OUTPUT = %q, want terraform=true", out)
			}
		})
	})

	t.Run("API failure is an error, never a false", func(t *testing.T) {
		// The whole design. A defaulted `false` here turns a GitHub blip into a
		// silently skipped state-import on every PR, which looks exactly like a
		// clean tree.
		listChangedFiles = func(string) ([]string, error) { return nil, errBoom{} }
		inTempRepo(t, func(dir string) {
			var errBuf bytes.Buffer
			c := PRTouchesCmd()
			c.SetOut(&bytes.Buffer{})
			c.SilenceUsage, c.SilenceErrors = true, true
			err := runCmd(t, c, []string{"--base-sha", "abc123", "--prefix", "x/"}, &errBuf)
			if err == nil {
				t.Fatal("an API failure must fail the command")
			}
			if !strings.Contains(err.Error(), "could not tell") {
				t.Errorf("error must distinguish 'could not tell' from 'nothing changed', got: %v", err)
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "out")); len(b) != 0 {
				t.Errorf("nothing may be written to GITHUB_OUTPUT on failure, got %q", b)
			}
		})
	})
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

func TestPRTouchesRequiresABaseSHA(t *testing.T) {
	// Without it there is nothing to diff against, and every PR would classify the
	// same way — a gate that always answers identically is not a gate.
	inTempRepo(t, func(string) {
		var errBuf bytes.Buffer
		c := PRTouchesCmd()
		c.SetOut(&bytes.Buffer{})
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := runCmd(t, c, []string{"--prefix", "x/"}, &errBuf); err == nil {
			t.Error("a missing --base-sha must be refused rather than guessed")
		}
	})
}

// ── upgrade-pr ──────────────────────────────────────────────────────────────

func TestUpgradePRRequiresBeforeSHA(t *testing.T) {
	// Without it there is no way to tell an upgrade from an instance that was
	// already current, so the command would open an empty PR every month.
	inTempRepo(t, func(string) {
		t.Setenv("GH_TOKEN", "x")
		var errBuf bytes.Buffer
		c := UpgradePRCmd()
		c.SilenceUsage, c.SilenceErrors = true, true
		err := runCmd(t, c, nil, &errBuf)
		if err == nil || !strings.Contains(err.Error(), "--before is required") {
			t.Errorf("missing --before must be refused, got %v", err)
		}
	})
}

func TestUpgradePRNamesTheMissingTokenBeforeDoingAnything(t *testing.T) {
	// Named up front rather than left to `gh pr create`, which would fail forty
	// seconds later with an auth message — by which time a branch has been pushed.
	inTempRepo(t, func(string) {
		t.Setenv("GH_TOKEN", "")
		t.Setenv("GITHUB_TOKEN", "")
		var errBuf bytes.Buffer
		c := UpgradePRCmd()
		c.SilenceUsage, c.SilenceErrors = true, true
		err := runCmd(t, c, []string{"--before", "abc"}, &errBuf)
		if err == nil || !strings.Contains(err.Error(), "LLZ_AUTOMATION_TOKEN") {
			t.Errorf("a missing token must name the secret, got %v", err)
		}
	})
}

func TestUpgradePROpensNothingWhenHEADDidNotMove(t *testing.T) {
	// The no-op path must touch neither the remote nor the PR API — and must
	// still exit 0, because "already current" is a correct outcome.
	origGit, origRemote, origPush, origCreate := gitOut, remoteHasBranch, pushBranch, createPR
	t.Cleanup(func() { gitOut, remoteHasBranch, pushBranch, createPR = origGit, origRemote, origPush, origCreate })

	gitOut = func(args ...string) (string, error) {
		switch {
		case args[0] == "rev-parse":
			return "abc\n", nil
		case args[0] == "status":
			return "", nil
		}
		return "v1.2.3\n", nil
	}
	remoteHasBranch = func(string) bool { t.Error("must not consult the remote when HEAD did not move"); return false }
	pushBranch = func(string) error { t.Error("must not push when HEAD did not move"); return nil }
	createPR = func(_, _, _, _ string) error { t.Error("must not open a PR when HEAD did not move"); return nil }

	inTempRepo(t, func(dir string) {
		t.Setenv("GH_TOKEN", "x")
		var errBuf bytes.Buffer
		c := UpgradePRCmd()
		if err := runCmd(t, c, []string{"--before", "abc", "--base", "main"}, &errBuf); err != nil {
			t.Fatalf("already-current must exit 0, got %v", err)
		}
		if !strings.Contains(errBuf.String(), "already on the target release") {
			t.Errorf("must say why no PR was opened, got: %s", errBuf.String())
		}
		s, _ := os.ReadFile(filepath.Join(dir, "summary"))
		if !strings.Contains(string(s), "opened a pull request: `false`") {
			t.Errorf("summary must record that no PR was opened, got %q", s)
		}
	})
}

func TestUpgradePRPushesAndOpensWhenHEADMoved(t *testing.T) {
	origGit, origRemote, origPush, origCreate := gitOut, remoteHasBranch, pushBranch, createPR
	t.Cleanup(func() { gitOut, remoteHasBranch, pushBranch, createPR = origGit, origRemote, origPush, origCreate })

	gitOut = func(args ...string) (string, error) {
		switch args[0] {
		case "rev-parse":
			return "def\n", nil
		case "status":
			return "", nil
		}
		// A git tag must NEVER reach the branch name: an instance repo carries the
		// ADOPTER's tags, not llz's, and naming the branch after one makes it
		// identical between llz releases — so the second upgrade finds the first
		// branch still open and declines forever. This stub returns one to prove it
		// is ignored.
		return "v0.0.0-adopter-tag\n", nil
	}
	remoteHasBranch = func(string) bool { return false }

	var pushed, title, base, head string
	pushBranch = func(b string) error { pushed = b; return nil }
	createPR = func(ti, _, ba, he string) error { title, base, head = ti, ba, he; return nil }

	inTempRepo(t, func(dir string) {
		writeAnswers(t, dir, "v9.9.9")
		t.Setenv("GH_TOKEN", "x")
		var errBuf bytes.Buffer
		c := UpgradePRCmd()
		if err := runCmd(t, c, []string{"--before", "abc", "--base", "main"}, &errBuf); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pushed != "chore/template-upgrade-v9.9.9" || head != pushed {
			t.Errorf("pushed %q, PR head %q", pushed, head)
		}
		if base != "main" {
			t.Errorf("base = %q, want main", base)
		}
		if !strings.Contains(title, "v9.9.9") {
			t.Errorf("title must name the version, got %q", title)
		}
	})
}

func TestUpgradePRLeavesAnExistingRemoteBranchAlone(t *testing.T) {
	// An earlier run's unmerged PR. Force-pushing over it would replace a diff
	// someone may be halfway through reviewing.
	origGit, origRemote, origPush, origCreate := gitOut, remoteHasBranch, pushBranch, createPR
	t.Cleanup(func() { gitOut, remoteHasBranch, pushBranch, createPR = origGit, origRemote, origPush, origCreate })

	gitOut = func(args ...string) (string, error) {
		switch args[0] {
		case "rev-parse":
			return "def\n", nil
		case "status":
			return "", nil
		}
		return "v1\n", nil
	}
	remoteHasBranch = func(string) bool { return true }
	pushBranch = func(string) error { t.Error("must not push over an existing remote branch"); return nil }
	createPR = func(_, _, _, _ string) error { t.Error("must not open a second PR"); return nil }

	inTempRepo(t, func(string) {
		t.Setenv("GH_TOKEN", "x")
		var errBuf bytes.Buffer
		c := UpgradePRCmd()
		if err := runCmd(t, c, []string{"--before", "abc", "--base", "main"}, &errBuf); err != nil {
			t.Fatalf("an already-open upgrade must exit 0, got %v", err)
		}
		if !strings.Contains(errBuf.String(), "not been merged") {
			t.Errorf("must explain why it stopped, got: %s", errBuf.String())
		}
	})
}

func TestPinnedVersionReadsTheAnswersFile(t *testing.T) {
	// An instance repo carries no llz git tags, so `git describe` cannot answer —
	// the pin the upgrade just rewrote is the version that matters.
	inTempRepo(t, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"),
			[]byte("_commit: abc\nllz_version: \"v4.5.6\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := pinnedVersion(); got != "v4.5.6" {
			t.Errorf("pinnedVersion() = %q, want v4.5.6", got)
		}
	})
	inTempRepo(t, func(string) {
		if got := pinnedVersion(); got != "unknown" {
			t.Errorf("a missing answers file must degrade to %q, got %q", "unknown", got)
		}
	})
}
