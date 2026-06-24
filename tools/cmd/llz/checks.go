package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// checks.go ports the instance-local checks that used to live in the template's
// Makefile into llz, so they propagate with the binary instead of via copier
// update. The lint configs (.tflintrc.hcl / .checkov.yaml / .gitleaks.toml) still
// ship in the instance — the underlying tools read them.
//
// Philosophy preserved from the Makefile: a missing tool SKIPS (with a warning)
// rather than blocking, so an absent linter never wedges a commit.

// candidateTFDirs are the Terraform roots an instance may carry. tfDirs() keeps
// only the ones that exist (the Makefile used `$(wildcard ...)` for this).
var candidateTFDirs = []string{
	"terraform-iac-bootstrap/cluster",
	"terraform-iac-bootstrap/cluster-bootstrap",
	"terraform-iac-bootstrap/object-storage",
	"terraform-iac-bootstrap/openbao-config",
}

// tfDirs returns the candidate Terraform roots that exist as directories.
func tfDirs() []string {
	var dirs []string
	for _, d := range candidateTFDirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// tool resolves the executable name for a check, honoring an env override so
// operators can point at a wrapper or pinned binary (mirrors the Makefile's
// `TOFU ?= tofu` overridable vars). e.g. tool("tofu", "LLZ_TOFU").
func tool(name, env string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return name
}

// haveTool reports whether bin is on PATH; when absent it prints the skip notice
// (matching the Makefile's `command -v … || { echo "  skip: …"; }`) so the caller
// can short-circuit the step as a pass.
func haveTool(bin string) bool {
	if _, err := execLookPath(bin); err != nil {
		fmt.Fprintf(os.Stderr, "  skip: %s not installed\n", bin)
		return false
	}
	return true
}

// ── argv builders (pure; covered by checks_test.go) ──────────────────────────

func fmtArgv(tofu, dir string) []string { return []string{tofu, "fmt", dir} }

func fmtCheckArgv(tofu, dir string) []string { return []string{tofu, "fmt", "-check", dir} }

// fmtCheckArgvPaths fmt-checks an explicit set of files instead of a whole dir,
// so generated (gitignored, untracked) per-env tfvars are skipped — see
// stepFmtCheck.
func fmtCheckArgvPaths(tofu string, paths []string) []string {
	return append([]string{tofu, "fmt", "-check"}, paths...)
}

// trackedFmtTargets returns the git-tracked *.tf / *.tfvars files under dir. It is
// how stepFmtCheck skips the rendered per-env tfvars: those are gitignored build
// artifacts (terraform-iac-bootstrap/.gitignore), so they are untracked and never
// listed — while committed modules (*.tf) and terraform.tfvars.example stay
// checked. Returns (nil, false) when not in a git repo so the caller falls back to
// the dir scan. A legacy instance's hand-committed <env>.tfvars ARE tracked, so
// they keep being checked — exactly right.
func trackedFmtTargets(dir string) ([]string, bool) {
	out, err := gitOutput("", "ls-files", "--", dir)
	if err != nil {
		return nil, false
	}
	var paths []string
	for _, p := range strings.Split(strings.TrimSpace(out), "\n") {
		if p = strings.TrimSpace(p); strings.HasSuffix(p, ".tf") || strings.HasSuffix(p, ".tfvars") {
			paths = append(paths, p)
		}
	}
	return paths, true
}

func tfLintArgv(tflint, dir, config string) []string {
	return []string{tflint, "--chdir=" + dir, "--config=" + config}
}

func actionsLintArgv(actionlint string, files []string) []string {
	return append([]string{actionlint}, files...)
}

func gitleaksArgv(gitleaks string) []string {
	return []string{gitleaks, "detect", "--source", ".", "--no-banner"}
}

func tfInitArgv(terraform, dir string) []string {
	return []string{terraform, "-chdir=" + dir, "init", "-backend=false", "-input=false"}
}

func tfValidateArgv(terraform, dir string) []string {
	return []string{terraform, "-chdir=" + dir, "validate"}
}

func checkovArgv(checkov, dir string) []string {
	return []string{checkov, "-d", dir, "--framework", "terraform",
		"--config-file", ".checkov.yaml", "--compact", "--quiet"}
}

// ── steps (each respects --dry-run via run; a missing tool is a no-op pass) ───

func stepFmtCheck(g globalOpts) error {
	tofu := tool("tofu", "LLZ_TOFU")
	if !haveTool(tofu) {
		return nil
	}
	for _, d := range tfDirs() {
		// Prefer fmt-checking only git-tracked files so the rendered per-env tfvars
		// (gitignored build artifacts) are skipped — an unformatted render must not
		// fail this pre-commit gate. Outside a git repo, fall back to the dir scan.
		if paths, ok := trackedFmtTargets(d); ok {
			if len(paths) == 0 {
				continue
			}
			if err := run(g, fmtCheckArgvPaths(tofu, paths)...); err != nil {
				return err
			}
			continue
		}
		if err := run(g, fmtCheckArgv(tofu, d)...); err != nil {
			return err
		}
	}
	return nil
}

func stepFmtFix(g globalOpts) error {
	tofu := tool("tofu", "LLZ_TOFU")
	if !haveTool(tofu) {
		return nil
	}
	for _, d := range tfDirs() {
		if err := run(g, fmtArgv(tofu, d)...); err != nil {
			return err
		}
	}
	return nil
}

func stepTFLint(g globalOpts) error {
	tflint := tool("tflint", "LLZ_TFLINT")
	if !haveTool(tflint) {
		return nil
	}
	// The Makefile passed an absolute --config so each --chdir'd root reads the
	// instance-root .tflintrc.hcl.
	config, err := filepath.Abs(".tflintrc.hcl")
	if err != nil {
		return err
	}
	for _, d := range tfDirs() {
		if err := run(g, tfLintArgv(tflint, d, config)...); err != nil {
			return err
		}
	}
	return nil
}

func stepActionsLint(g globalOpts) error {
	actionlint := tool("actionlint", "LLZ_ACTIONLINT")
	if !haveTool(actionlint) {
		return nil
	}
	files, err := filepath.Glob(".github/workflows/*.yml")
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	return run(g, actionsLintArgv(actionlint, files)...)
}

func stepGitleaks(g globalOpts) error {
	gitleaks := tool("gitleaks", "LLZ_GITLEAKS")
	if !haveTool(gitleaks) {
		return nil
	}
	return run(g, gitleaksArgv(gitleaks)...)
}

func stepTFValidate(g globalOpts) error {
	terraform := tool("terraform", "LLZ_TERRAFORM")
	if !haveTool(terraform) {
		return nil
	}
	for _, d := range tfDirs() {
		if err := run(g, tfInitArgv(terraform, d)...); err != nil {
			return err
		}
		if err := run(g, tfValidateArgv(terraform, d)...); err != nil {
			return err
		}
	}
	return nil
}

func stepCheckov(g globalOpts) error {
	checkov := tool("checkov", "LLZ_CHECKOV")
	if !haveTool(checkov) {
		return nil
	}
	for _, d := range tfDirs() {
		if err := run(g, checkovArgv(checkov, d)...); err != nil {
			return err
		}
	}
	return nil
}

// runLint is the fast pre-commit gate (also called by `llz precommit`).
func runLint(g globalOpts) error {
	for _, step := range []func(globalOpts) error{
		stepFmtCheck, stepTFLint, stepActionsLint, stepGitleaks,
	} {
		if err := step(g); err != nil {
			return err
		}
	}
	fmt.Fprintln(os.Stderr, "lint: ok")
	return nil
}

func runValidate(g globalOpts) error {
	// The spec is config-as-code, so the code gate validates it first when present
	// (this is where `llz validate` users look for "is my spec valid?"). Same check
	// as `llz render --check`, run before the TF roots.
	if lz, present, err := loadSpec(); present {
		if err != nil {
			return err
		}
		if errs := lz.Validate(); len(errs) > 0 {
			fmt.Fprintf(os.Stderr, "LandingZone spec is invalid (%d problem(s)):\n", len(errs))
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "  • %v\n", e)
			}
			return fmt.Errorf("invalid LandingZone spec")
		}
		fmt.Fprintln(os.Stderr, "spec: ok")
	}
	for _, step := range []func(globalOpts) error{stepTFValidate, stepCheckov} {
		if err := step(g); err != nil {
			return err
		}
	}
	fmt.Fprintln(os.Stderr, "validate: ok")
	return nil
}

// ── commands ──────────────────────────────────────────────────────────────────

func lintCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lint",
		Short: "fast gate: tofu fmt-check + tflint + actionlint + gitleaks",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return runLint(gopts) },
	}
}

func fmtCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fmt",
		Short: "tofu fmt (auto-fix terraform formatting)",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return stepFmtFix(gopts) },
	}
}

func validateCmd() *cobra.Command {
	var env string
	c := &cobra.Command{
		Use:   "validate",
		Short: "code-level gate: LandingZone spec + terraform validate + checkov",
		Long: "Validates the LandingZone spec (when present) then runs terraform validate +\n" +
			"checkov across the TF roots — the deep, on-demand code gate (slower than\n" +
			"`llz lint`, the fast pre-commit gate). The spec check is the same as\n" +
			"`llz render --check`.\n\n" +
			"--env is DEPRECATED: deployment readiness is now part of the single\n" +
			"\"am I ready to build?\" gate, `llz doctor --env <env>` (tooling + gh auth +\n" +
			"file-level placeholders + repo config). `validate --env` still delegates to\n" +
			"that same scan for now, but prefer `llz doctor --env`.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if env != "" {
				// Thin back-compat alias — the readiness scan now lives in doctor.
				return runEnvReadiness(env)
			}
			return runValidate(gopts)
		},
	}
	c.Flags().StringVar(&env, "env", "", "DEPRECATED: use `llz doctor --env <env>` (delegates to the same readiness scan)")
	_ = c.Flags().MarkDeprecated("env", "use `llz doctor --env <env>` instead")
	return c
}

// checkCmd groups the individual steps for debugging a single check in isolation
// (the Makefile exposed each target separately). It is an advanced escape hatch —
// hidden from top-level help so newcomers reach for `llz lint` (fast gate) and
// `llz validate` (deep gate) instead; both run the same underlying step functions.
func checkCmd() *cobra.Command {
	c := &cobra.Command{
		Use:    "check",
		Short:  "run an individual check step in isolation (advanced)",
		Long:   "Runs one check step on its own — a debugging escape hatch. The everyday\nentrypoints are `llz lint` (fast pre-commit gate) and `llz validate` (deep\ncode gate); both dispatch to these same steps.",
		Hidden: true,
	}
	steps := []struct {
		use, short string
		fn         func(globalOpts) error
	}{
		{"fmt-check", "tofu fmt -check (no writes)", stepFmtCheck},
		{"tf-lint", "tflint each terraform/ root", stepTFLint},
		{"actions-lint", "actionlint the instance workflows", stepActionsLint},
		{"gitleaks", "gitleaks secret scan of the working tree", stepGitleaks},
		{"tf-validate", "terraform validate (init -backend=false per root)", stepTFValidate},
		{"checkov", "Checkov IaC security scan of the terraform/ roots", stepCheckov},
	}
	for _, s := range steps {
		s := s
		c.AddCommand(&cobra.Command{
			Use:   s.use,
			Short: s.short,
			Args:  cobra.NoArgs,
			RunE:  func(_ *cobra.Command, _ []string) error { return s.fn(gopts) },
		})
	}
	return c
}
