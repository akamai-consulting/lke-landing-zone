package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// hooks.go implements `llz hooks` (install the pre-commit hook) and the hidden
// `llz precommit` it invokes. The hook is a thin shim written into the repo's
// real hooks dir (.git/hooks by default) that exec's llz by the ABSOLUTE path of
// the installing binary — so commits run the checks even when llz isn't on PATH.
// It is a per-clone install (the shim isn't committed); `llz new` arms it on
// scaffold and operators re-run `llz hooks` in fresh clones.

// secretPatterns are the staged-path shapes the pre-commit secrets guard blocks
// (the high-frequency leak vectors for a landing-zone instance). Ported verbatim
// from the template's hand-written pre-commit hook.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\.(pem|der|key|p12|pfx)$`),
	regexp.MustCompile(`\.tfstate(\.backup)?$`),
	regexp.MustCompile(`(^|/)kubeconfig`),
	regexp.MustCompile(`(^|/)\.terraform/`),
}

// isSecretPath reports whether a staged path looks like sensitive material.
func isSecretPath(path string) bool {
	for _, re := range secretPatterns {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// preCommitShim renders the hook body. llzPath is the absolute path captured at
// install time; the PATH fallback keeps the hook working if that binary moves.
func preCommitShim(llzPath string) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
# Installed by `+"`llz hooks`"+` — regenerate with `+"`llz hooks`"+`. Do not edit;
# instance-specific checks go in .githooks/pre-commit.local instead.
if [ -x %q ]; then exec %q precommit "$@"; fi
command -v llz >/dev/null 2>&1 && exec llz precommit "$@"
echo "pre-commit: llz not found; skipping checks" >&2
`, llzPath, llzPath)
}

// gitOutput runs git in dir and returns trimmed stdout. `git -C dir` is
// equivalent to setting cmd.Dir and keeps the call on the execOutput seam.
func gitOutput(dir string, args ...string) (string, error) {
	out, err := execOutput("git", append([]string{"-C", dir}, args...)...)
	return strings.TrimSpace(string(out)), err
}

// runHooksInstall writes the pre-commit shim into dir's git hooks directory.
func runHooksInstall(g globalOpts, dir string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate llz binary: %w", err)
	}
	hooksDir, err := gitOutput(dir, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return fmt.Errorf("resolve git hooks dir (is %s a git repo?): %w", dir, err)
	}
	if !filepath.IsAbs(hooksDir) {
		hooksDir = filepath.Join(dir, hooksDir)
	}
	hookPath := filepath.Join(hooksDir, "pre-commit")
	shim := preCommitShim(self)

	if g.dryRun {
		fmt.Fprintf(os.Stderr, "→ (dry-run) would write %s:\n%s", hookPath, shim)
		return nil
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create hooks dir: %w", err)
	}
	if err := os.WriteFile(hookPath, []byte(shim), 0o755); err != nil {
		return fmt.Errorf("write %s: %w", hookPath, err)
	}
	fmt.Fprintf(os.Stderr, "armed pre-commit hook: %s → %s precommit\n", hookPath, self)
	return nil
}

// runPrecommit is the hook entrypoint: secrets guard on staged files, then the
// fast lint gate, then the optional operator escape hatch.
func runPrecommit(g globalOpts) error {
	root, err := gitOutput(".", "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("not in a git repo: %w", err)
	}
	if err := os.Chdir(root); err != nil {
		return err
	}

	staged, err := gitOutput(".", "diff", "--cached", "--name-only", "--diff-filter=ACMR")
	if err != nil {
		return fmt.Errorf("list staged files: %w", err)
	}
	if staged == "" {
		return nil
	}
	files := strings.Split(staged, "\n")

	// ── secrets guard ──
	var blocked []string
	for _, f := range files {
		if isSecretPath(f) {
			blocked = append(blocked, f)
		}
	}
	if len(blocked) > 0 {
		for _, f := range blocked {
			fmt.Fprintf(os.Stderr, "pre-commit: BLOCKED — refusing to commit sensitive file: %s\n", f)
		}
		fmt.Fprintln(os.Stderr, "  Unstage it: git reset HEAD <file> (and add it to .gitignore).")
		return fmt.Errorf("secrets guard blocked %d file(s)", len(blocked))
	}

	// ── lint ──
	fmt.Fprintln(os.Stderr, "pre-commit: running llz lint")
	if err := runLint(g); err != nil {
		return err
	}

	// ── operator escape hatch ──
	if fi, err := os.Stat(".githooks/pre-commit.local"); err == nil && fi.Mode()&0o111 != 0 {
		return run(g, ".githooks/pre-commit.local")
	}
	return nil
}

// ── commands ──────────────────────────────────────────────────────────────────

func hooksCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "hooks",
		Short: "install the pre-commit hook (secrets guard + llz lint)",
		Long: "Writes a pre-commit shim into this repo's git hooks dir that runs the\n" +
			"secrets guard + `llz lint` on every commit. The shim exec's llz by absolute\n" +
			"path, so it works even when llz isn't on $PATH. Re-run in each fresh clone.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runHooksInstall(gopts, ".") },
	}
}

func precommitCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "precommit",
		Short:  "pre-commit hook entrypoint (invoked by the installed git hook)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE:   func(_ *cobra.Command, _ []string) error { return runPrecommit(gopts) },
	}
}
