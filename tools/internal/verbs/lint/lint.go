package lint

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/configreadiness"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/manifestguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/pincoherence"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/render"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghcli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/gitcmd"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/manifest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/proc"
	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/sustain"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfbin"
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
// `databases` is here because an instance that declares a Managed Postgres
// carries that root exactly like the other three, and omitting it meant tflint,
// checkov and tf-validate all silently skipped it — a root can only be linted by
// a list it appears on, and nothing failed to say it was missing.
var candidateTFDirs = []string{
	"terraform-iac-bootstrap/cluster",
	"terraform-iac-bootstrap/databases",
	"terraform-iac-bootstrap/object-storage",
	"terraform-iac-bootstrap/vpc",
}

// tfDirs returns the candidate Terraform roots that exist as directories.
func tfDirs() []string {
	var dirs []string
	for _, d := range candidateTFDirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			dirs = append(dirs, d)
		}
	}
	// COUNT HCL, NOT DIRECTORIES, and the difference is the whole value of
	// --strict. Every real instance checkout HAS terraform-iac-bootstrap/cluster
	// and object-storage — each holds a tracked .terraform.lock.hcl — while
	// carrying zero *.tf, because the roots are rendered. A directory census
	// therefore says "2 roots found" about a tree with nothing in it to lint, and
	// --strict would have signed off on exactly the vacuous green it was added to
	// refuse. What the linters actually read is the only thing worth counting.
	scan.usedTFDirs, scan.tfDirCount, scan.tfFileCount = true, len(dirs), countTFFiles(dirs)
	return dirs
}

// countTFFiles totals the *.tf across the given roots.
func countTFFiles(dirs []string) int {
	n := 0
	for _, d := range dirs {
		matches, err := filepath.Glob(filepath.Join(d, "*.tf"))
		if err == nil {
			n += len(matches)
		}
	}
	return n
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
		scan.missingTools = append(scan.missingTools, bin)
		return false
	}
	return true
}

// scan records what the step that just ran actually EXAMINED, so `--strict` can
// refuse a check that passed having looked at nothing.
//
// A MISSING TOOL AND AN EMPTY ROOT SET BOTH EXIT 0 HERE, by design: this package
// began as the pre-commit gate, where an absent linter must never wedge a commit.
// In a delivered CI job that default is the wrong one and produces exactly the
// vacuous green this tree keeps meeting — `llz check tf-lint` on an instance
// whose TF_IMAGE lost tflint, or before the roots are rendered, reports SUCCESS
// having scanned nothing, and the release-e2e PR-gate probe then certifies that
// success. Same command, two callers, opposite correct answers; the flag is what
// separates them.
var scan struct {
	missingTools []string
	usedTFDirs   bool
	tfDirCount   int
	tfFileCount  int
	// usedTargets/targetCount are the generic form of the same census, for steps
	// that scan something other than Terraform roots (Go dirs, workflow files).
	// Without them --strict protected only tf-lint and checkov while reading as a
	// property of `llz check`.
	usedTargets bool
	targetCount int
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
	out, err := gitcmd.Output("", "ls-files", "--", dir)
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

// goFmtListArgv lists Go files needing formatting. `gofmt -l` prints paths and
// exits 0 either way, so the OUTPUT is the verdict — not the exit code.
func goFmtListArgv(gofmtBin string, dirs []string) []string {
	return append([]string{gofmtBin, "-l"}, dirs...)
}

// goFmtUnformatted parses `gofmt -l` output into the list of files needing
// formatting. Pure, so the parsing is tested without invoking gofmt.
func goFmtUnformatted(out string) []string {
	var files []string
	for _, ln := range strings.Split(out, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			files = append(files, ln)
		}
	}
	return files
}

// goModuleDirs are the module roots stepGoFmt formats, relative to the repo root.
// Only the ones that exist are scanned, so this is a no-op in an adopter instance
// repo (which vendors no Go source).
func goModuleDirs() []string {
	var out []string
	for _, d := range []string{"tools/cmd", "tools/internal"} {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// ── steps (each respects --dry-run via run; a missing tool is a no-op pass) ───

// stepGoFmt is the gofmt half of the format gate. stepFmtCheck covers HCL via
// `tofu fmt`; nothing covered Go, so gofmt drift could only ever be caught by
// CI's `make fmt-check` — which is exactly how it was caught, as a color.Red Lint on an
// already-pushed commit. The pre-commit gate is the cheaper place to learn it.
//
// Deliberately NOT `make fmt-check`: this must run from any cwd inside the repo,
// stay a no-op where there is no Go tree, and not depend on make.
func stepGoFmt(g cliopts.Opts) error {
	gofmtBin := tool("gofmt", "LLZ_GOFMT")
	dirs := goModuleDirs()
	// ORDER MATTERS FOR --strict: `||` short-circuits, so testing the empty dir set
	// first meant haveTool never ran and a missing gofmt was never RECORDED — the
	// vacuity census saw a clean pass. Ask about the tool unconditionally, then
	// record an empty target set the same way tfDirs does.
	haveGofmt := haveTool(gofmtBin)
	scan.usedTargets, scan.targetCount = true, len(dirs)
	if len(dirs) == 0 || !haveGofmt {
		return nil
	}
	argv := goFmtListArgv(gofmtBin, dirs)
	fmt.Fprintln(os.Stderr, "→ "+ghcli.Quote(argv))
	if g.DryRun {
		return nil
	}
	out, _ := cigate.RunCombined(exec.Command(argv[0], argv[1:]...))
	unformatted := goFmtUnformatted(out)
	if len(unformatted) == 0 {
		return nil
	}
	fmt.Fprintf(os.Stderr, "gofmt: %d file(s) need formatting:\n", len(unformatted))
	for _, f := range unformatted {
		fmt.Fprintf(os.Stderr, "  • %s\n", f)
	}
	fmt.Fprintf(os.Stderr, "  fix: gofmt -w %s\n", strings.Join(dirs, " "))
	return fmt.Errorf("gofmt: %d file(s) need formatting", len(unformatted))
}

func stepFmtCheck(g cliopts.Opts) error {
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
			if err := proc.RunEcho(g.DryRun, fmtCheckArgvPaths(tofu, paths)...); err != nil {
				return err
			}
			continue
		}
		if err := proc.RunEcho(g.DryRun, fmtCheckArgv(tofu, d)...); err != nil {
			return err
		}
	}
	return nil
}

func stepFmtFix(g cliopts.Opts) error {
	tofu := tool("tofu", "LLZ_TOFU")
	if !haveTool(tofu) {
		return nil
	}
	for _, d := range tfDirs() {
		if err := proc.RunEcho(g.DryRun, fmtArgv(tofu, d)...); err != nil {
			return err
		}
	}
	return nil
}

func stepTFLint(g cliopts.Opts) error {
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
		if err := proc.RunEcho(g.DryRun, tfLintArgv(tflint, d, config)...); err != nil {
			return err
		}
	}
	return nil
}

func stepActionsLint(g cliopts.Opts) error {
	actionlint := tool("actionlint", "LLZ_ACTIONLINT")
	if !haveTool(actionlint) {
		return nil
	}
	files, err := filepath.Glob(".github/workflows/*.yml")
	if err != nil {
		return err
	}
	// Recorded so --strict can refuse a run that linted no workflows at all.
	scan.usedTargets, scan.targetCount = true, len(files)
	if len(files) == 0 {
		return nil
	}
	return proc.RunEcho(g.DryRun, actionsLintArgv(actionlint, files)...)
}

func stepGitleaks(g cliopts.Opts) error {
	gitleaks := tool("gitleaks", "LLZ_GITLEAKS")
	if !haveTool(gitleaks) {
		return nil
	}
	return proc.RunEcho(g.DryRun, gitleaksArgv(gitleaks)...)
}

// conflictMarkerLines scans text for git/copier merge-conflict markers and
// returns the 1-based line numbers of any it finds. It flags ONLY the
// unambiguous 7-character start (`<<<<<<<`) and end (`>>>>>>>`) markers — a bare
// line, or one followed by a space + label (git's `<<<<<<< HEAD`, copier's
// `<<<<<<< before updating` / `>>>>>>> after updating`). It deliberately does
// NOT flag the `=======` middle marker on its own: seven equals signs appear
// legitimately (Markdown setext underlines, horizontal rules), so treating them
// as a conflict would false-positive on docs. A real conflict always carries a
// start + end marker, so nothing is missed. A run of 8+ `<`/`>` (e.g. ASCII art)
// is not a marker and is not flagged.
func conflictMarkerLines(content string) []int {
	var lines []int
	for i, ln := range strings.Split(content, "\n") {
		ln = strings.TrimRight(ln, "\r")
		for _, m := range []string{"<<<<<<<", ">>>>>>>"} {
			if ln == m || strings.HasPrefix(ln, m+" ") {
				lines = append(lines, i+1)
			}
		}
	}
	return lines
}

// stepVendoredFresh fails the lint gate when a vendored, template-owned file
// under .github/ has been hand-edited. Those files are `managed`, so `llz upgrade`
// overwrites them from a clean render — an operator's local fix is silently lost
// on the next bump, which is precisely the drift the cross-org reuse design said
// should "fail CI rather than silently diverge".
//
// Lives in the LINT gate rather than a workflow step on purpose: lint already runs
// in every instance's CI and pre-commit hook, so the guard reaches instances
// without editing (and thereby churning) the vendored workflows it protects.
//
// Skips cleanly when there is no .template-manifest / no lock — a template-repo
// checkout or a pre-lock instance has nothing to verify.
func stepVendoredFresh(_ cliopts.Opts) error {
	if _, err := manifest.Load(""); err != nil {
		fmt.Fprintln(os.Stderr, "  skip: no .template-manifest (vendored-fresh)")
		return nil
	}
	return sustain.RunManagedFresh(SustainDeps(), "", false, io.Discard, os.Stderr)
}

// stepRenderFresh fails the lint gate when the COMMITTED render output no longer
// matches what the spec renders — `llz render --check`, which existed but was
// wired into nothing an instance runs.
//
// That gap is not theoretical. The shared platform-apl tree is fetched at the
// instance's pin (resolveTemplateRef reads .copier-answers.yml), so every upgrade
// silently invalidates the committed apl-values kustomizations until someone
// re-renders — and nothing said so. A live instance ran three releases behind:
// ArgoCD was fetching v0.0.31 manifests under a v0.0.34 instance, which is a
// difference in what is DEPLOYED, not just what is checked in.
//
// Skips outside an instance (the template repo has no spec of its own).
// stepPinCoherence is the lint-gate wrapper. It runs in an instance's pre-commit
// hook (where an upgrade's diff is about to be committed) and is a no-op in the
// template repo, which has no .copier-answers.yml of its own.
func stepPinCoherence(_ cliopts.Opts) error { return pincoherence.Assert(".") }

func stepRenderFresh(g cliopts.Opts) error {
	tfDir, _, _ := instancelayout.Detect()
	if !clusterspec.InstancePresent(filepath.Dir(tfDir)) {
		return nil // no LandingZone spec — nothing renders here
	}
	if err := render.Run(g.DryRun, "", false, true, false); err != nil {
		return fmt.Errorf("committed render output is stale (`llz render` to refresh): %w", err)
	}
	return nil
}

// stepConflictMarkers fails the lint gate if any git-tracked text file carries a
// committed merge-conflict marker. A botched `copier update` / `llz upgrade`
// 3-way merge can leave these in place (e.g. an instance's kustomization.yaml),
// producing invalid YAML that only surfaces when Argo/kustomize chokes far
// downstream — the exact silent-breakage class Phase 0 of the cross-org reuse
// pattern closes (docs/designs/cross-org-reuse-pattern.md). Native (no external
// tool), so the only legitimate skip is "not a git repo".
//
// It used to skip on ANY gitcmd.Output error while the comment claimed it "can never
// skip" — so a corrupt index, a permissions problem, or git missing from PATH
// silently passed a scan that exists precisely to stop silent breakage. Now the
// two are told apart: no repo is a skip, a repo we cannot read is an error.
func stepConflictMarkers(_ cliopts.Opts) error {
	out, err := gitcmd.Output("", "ls-files")
	if err != nil {
		if _, repoErr := gitcmd.Output("", "rev-parse", "--git-dir"); repoErr != nil {
			fmt.Fprintln(os.Stderr, "  skip: not a git repo (conflict-marker scan)")
			return nil
		}
		return fmt.Errorf("conflict-marker scan: this IS a git repo but `git ls-files` failed, "+
			"so nothing was scanned — refusing to report clean: %w", err)
	}
	var hits []string
	for _, f := range strings.Split(out, "\n") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue // race with a delete / unreadable — not this check's concern
		}
		if bytes.IndexByte(data, 0) >= 0 {
			continue // binary file — skip
		}
		for _, ln := range conflictMarkerLines(string(data)) {
			hits = append(hits, fmt.Sprintf("%s:%d", f, ln))
		}
	}
	if len(hits) > 0 {
		fmt.Fprintf(os.Stderr, "conflict markers found in %d location(s):\n", len(hits))
		for _, h := range hits {
			fmt.Fprintf(os.Stderr, "  • %s\n", h)
		}
		return fmt.Errorf("committed merge-conflict markers — resolve them before committing " +
			"(a 3-way `copier update`/`llz upgrade` merge likely left them; see " +
			"docs/designs/cross-org-reuse-pattern.md Phase 0)")
	}
	return nil
}

// StepDroppedAPIVersions fails when a manifest under scannedManifestTrees declares
// an apiVersion the cluster no longer serves (see droppedAPIs).
//
// Tolerates an empty corpus: an instance repo legitimately has no platform-apl/ and
// may have no custom YAML at all. The CI face (`llz ci dropped-apiversions`) runs in
// the template, where empty means the trees moved — and refuses to pass on it.
func stepDroppedAPIVersions(_ cliopts.Opts) error {
	hits, _, err := manifestguard.ScanDroppedAPIVersions("")
	if err != nil {
		return err
	}
	return manifestguard.ReportDroppedAPIVersions(hits)
}

func stepTFValidate(g cliopts.Opts) error {
	terraform := tool(tfbin.Bin(), "LLZ_TERRAFORM")
	if !haveTool(terraform) {
		return nil
	}
	for _, d := range tfDirs() {
		if err := proc.RunEcho(g.DryRun, tfInitArgv(terraform, d)...); err != nil {
			return err
		}
		if err := proc.RunEcho(g.DryRun, tfValidateArgv(terraform, d)...); err != nil {
			return err
		}
	}
	return nil
}

func stepCheckov(g cliopts.Opts) error {
	checkov := tool("checkov", "LLZ_CHECKOV")
	if !haveTool(checkov) {
		return nil
	}
	for _, d := range tfDirs() {
		if err := proc.RunEcho(g.DryRun, checkovArgv(checkov, d)...); err != nil {
			return err
		}
	}
	return nil
}

// lintSteps is the ordered gate. Named (not inlined into RunLint) so a test can
// assert a step is actually WIRED IN — an unreferenced check protects nothing,
// and that is a silent failure no amount of testing the step itself would catch.
func lintSteps() []func(cliopts.Opts) error {
	return []func(cliopts.Opts) error{
		stepConflictMarkers, stepDroppedAPIVersions, stepVendoredFresh, func(g cliopts.Opts) error { return sustain.StepUpgradeChurnGuard(SustainDeps()) },
		stepPinCoherence, stepRenderFresh,
		stepFmtCheck, stepGoFmt, stepTFLint, stepActionsLint, stepGitleaks,
	}
}

// RunLint is the fast pre-commit gate (also called by `llz precommit`).
func RunLint(g cliopts.Opts) error {
	for _, step := range lintSteps() {
		if err := step(g); err != nil {
			return err
		}
	}
	fmt.Fprintln(os.Stderr, "lint: ok")
	return nil
}

func RunValidate(g cliopts.Opts) error {
	// The spec is config-as-code, so the code gate validates it first when present
	// (this is where `llz validate` users look for "is my spec valid?"). Same check
	// as `llz render --check`, run before the TF roots.
	if lz, present, err := clusterspec.Detected(); present {
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
	for _, step := range []func(cliopts.Opts) error{stepTFValidate, stepCheckov} {
		if err := step(g); err != nil {
			return err
		}
	}
	fmt.Fprintln(os.Stderr, "validate: ok")
	return nil
}

// ── commands ──────────────────────────────────────────────────────────────────

func LintCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lint",
		Short: "fast gate: tofu fmt-check + tflint + actionlint + gitleaks",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return RunLint(cliopts.Global) },
	}
}

func FmtCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fmt",
		Short: "tofu fmt (auto-fix terraform formatting)",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return stepFmtFix(cliopts.Global) },
	}
}

func ValidateCmd() *cobra.Command {
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
				return configreadiness.RunEnvReadiness(env)
			}
			return RunValidate(cliopts.Global)
		},
	}
	c.Flags().StringVar(&env, "env", "", "DEPRECATED: use `llz doctor --env <env>` (delegates to the same readiness scan)")
	_ = c.Flags().MarkDeprecated("env", "use `llz doctor --env <env>` instead")
	return c
}

// CheckCmd groups the individual steps for debugging a single check in isolation
// (the Makefile exposed each target separately). It is an advanced escape hatch —
// hidden from top-level help so newcomers reach for `llz lint` (fast gate) and
// `llz validate` (deep gate) instead; both run the same underlying step functions.
func CheckCmd() *cobra.Command {
	c := &cobra.Command{
		Use:    "check",
		Short:  "run an individual check step in isolation (advanced)",
		Long:   "Runs one check step on its own — a debugging escape hatch. The everyday\nentrypoints are `llz lint` (fast pre-commit gate) and `llz validate` (deep\ncode gate); both dispatch to these same steps.",
		Hidden: true,
	}
	steps := []struct {
		use, short string
		fn         func(cliopts.Opts) error
	}{
		{"conflict-markers", "fail on committed merge-conflict markers", stepConflictMarkers},
		{"vendored-fresh", "fail when a vendored .github/ file drifts from the template", stepVendoredFresh},
		{"pin-coherence", "fail when .copier-answers.yml's _commit and llz_version name different releases", stepPinCoherence},
		{"fmt-check", "tofu fmt -check (no writes)", stepFmtCheck},
		{"tf-lint", "tflint each terraform/ root", stepTFLint},
		{"actions-lint", "actionlint the instance workflows", stepActionsLint},
		{"gitleaks", "gitleaks secret scan of the working tree", stepGitleaks},
		{"tf-validate", "terraform validate (init -backend=false per root)", stepTFValidate},
		{"checkov", "Checkov IaC security scan of the terraform/ roots", stepCheckov},
	}
	var strict bool
	c.PersistentFlags().BoolVar(&strict, "strict", false,
		"fail instead of passing when the check could not examine anything — a linter missing from PATH, "+
			"or no *.tf on disk to read (delivered CI passes this; pre-commit must not)")
	for _, s := range steps {
		s := s
		c.AddCommand(&cobra.Command{
			Use:   s.use,
			Short: s.short,
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				scan.missingTools, scan.usedTFDirs = nil, false
				scan.tfDirCount, scan.tfFileCount = 0, 0
				scan.usedTargets, scan.targetCount = false, 0
				if err := s.fn(cliopts.Global); err != nil {
					return err
				}
				if !strict {
					return nil
				}
				cmd.SilenceUsage = true
				if len(scan.missingTools) > 0 {
					return fmt.Errorf("::error::%s could not run: %s not on PATH. Under --strict a check that "+
						"examined nothing is a FAILURE, not a pass — a green gate that scanned nothing is the "+
						"thing this flag exists to prevent. Fix the image (TF_IMAGE) rather than dropping --strict",
						s.use, strings.Join(scan.missingTools, ", "))
				}
				if scan.usedTargets && scan.targetCount == 0 {
					return fmt.Errorf("::error::%s found nothing to examine — no target files in this "+
						"checkout. Under --strict, scanning nothing is a FAILURE", s.use)
				}
				if scan.usedTFDirs && scan.tfFileCount == 0 {
					// TWO DIFFERENT TREES, and saying the wrong one wastes the
					// operator's first five minutes. The usual case is an
					// unrendered instance: the root DIRECTORIES are there (they
					// hold the tracked .terraform.lock.hcl) and hold no HCL. The
					// other is a checkout that has no terraform-iac-bootstrap/ at
					// all — a wrong working directory, or not an instance —
					// where telling someone "the root directories exist" is
					// simply false.
					where := "the root directories exist (they hold the tracked .terraform.lock.hcl) but hold no HCL yet"
					if scan.tfDirCount == 0 {
						where = "there is no terraform-iac-bootstrap/ root here at all — check the working directory, " +
							"or whether this is an instance checkout"
					}
					// AND SAY WHEN `llz render` CANNOT HELP, or the advice is a
					// loop. The delivered job renders with --if-spec, which
					// NO-OPS on an instance with no LandingZone spec — so telling
					// that operator to run the render sends them back to the
					// command that just did nothing. A pre-spec instance is
					// supposed to commit its Terraform; if it has neither a spec
					// nor committed roots, the tree is the problem, not the order
					// of the steps.
					fix := "An instance commits zero Terraform, so the roots arrive from " +
						"`llz render --tfvars-only --if-spec` — run it before this check."
					if !render.SpecPresent() {
						fix = "This instance has NO LandingZone spec, so `llz render` cannot produce the roots " +
							"(--if-spec makes it a no-op). A pre-spec instance is expected to COMMIT its " +
							"Terraform; finding neither a spec nor committed roots means the tree is " +
							"incomplete — adopt the spec (`llz env add`) or restore the committed roots."
					}
					return fmt.Errorf("::error::%s found no *.tf to scan under terraform-iac-bootstrap/: %s. %s "+
						"Under --strict, scanning nothing is a FAILURE", s.use, where, fix)
				}
				return nil
			},
		})
	}
	return c
}
