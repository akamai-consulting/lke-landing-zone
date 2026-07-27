package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
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
func stepVendoredFresh(_ globalOpts) error {
	if _, err := loadTemplateManifest(""); err != nil {
		fmt.Fprintln(os.Stderr, "  skip: no .template-manifest (vendored-fresh)")
		return nil
	}
	return runWorkflowsFresh("", false, io.Discard, os.Stderr)
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
func stepRenderFresh(g globalOpts) error {
	tfDir, _, _ := instanceLayout()
	if !clusterspec.InstancePresent(filepath.Dir(tfDir)) {
		return nil // no LandingZone spec — nothing renders here
	}
	if err := runRender(g, "", false, true, false); err != nil {
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
// It used to skip on ANY gitOutput error while the comment claimed it "can never
// skip" — so a corrupt index, a permissions problem, or git missing from PATH
// silently passed a scan that exists precisely to stop silent breakage. Now the
// two are told apart: no repo is a skip, a repo we cannot read is an error.
func stepConflictMarkers(_ globalOpts) error {
	out, err := gitOutput("", "ls-files")
	if err != nil {
		if _, repoErr := gitOutput("", "rev-parse", "--git-dir"); repoErr != nil {
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

// droppedAPIs are apiVersions a converged apl-core dependency no longer serves, so
// a manifest still declaring one fails to apply ("no matches for kind … in version
// …") and surfaces only as an opaque Argo SyncFailed at deploy. Add an entry when a
// dependency drops one.
//
// Verify `served: false` (not merely "a newer version exists") before adding a row
// — a CRD commonly keeps an old version listed but unserved:
//
//	helm template eso external-secrets/external-secrets --version <v> --set crds.create=true |
//	  yq 'select(.kind=="CustomResourceDefinition") | .spec.names.kind + " " +
//	      .spec.versions[].name + " served=" + (.spec.versions[].served|tostring)'
var droppedAPIs = []struct{ apiVersion, since, fix string }{
	{
		apiVersion: "external-secrets.io/v1beta1",
		since:      "apl-core v6 (bundled external-secrets 2.4.1)",
		fix:        "bump to external-secrets.io/v1 (drop-in for standard ExternalSecret specs)",
	},
	// NB: external-secrets.io/v1alpha1 is NOT dropped — PushSecret/ClusterPushSecret
	// and the generators are v1alpha1-only in 2.4.1 (served, and the storage version).
	// platform-apl/components/harbor/harbor-admin-push.yaml is correct as-is.
}

// scannedManifestTrees are the manifest roots the dropped-apiVersion guard walks.
// Two classes, each uncovered by every other gate:
//
//   - Operator-OWNED escape hatches (kubernetes-custom/, kubernetes-charts/).
//     `llz upgrade` deliberately never rewrites these, so a dependency's version
//     drop leaves them stale until the operator migrates them by hand.
//   - The template's own SHARED tree (platform-apl/). No CRD-aware gate reads it:
//     k8s-lint, k8s-validate and the kind server-side dry-run all scan $RENDER_DIR,
//     which render-charts.sh builds from kubernetes-charts/*/ ONLY. Every instance
//     fetches platform-apl/ remotely (clusterspec's kustomize remoteBasePrefix), so
//     one stale apiVersion here breaks every instance at once with nothing upstream
//     to catch it — exactly how llz-cidr-firewall's ExternalSecret shipped on
//     external-secrets.io/v1beta1.
//
// Prefixes are repo-root-relative and cover both layouts: an instance repo carries
// kubernetes-custom/ at its root, the template carries the scaffold copy under
// instance-template/.
var scannedManifestTrees = []string{
	"kubernetes-custom/",
	"instance-template/kubernetes-custom/",
	"kubernetes-charts/",
	"platform-apl/",
}

// isDeclaredAPIVersion reports whether a YAML line declares `apiVersion: <api>`
// (allowing indentation, quotes, and a trailing comment). It matches only a real
// manifest key, so a prose/comment mention (e.g. a changelog "… v1beta1 → v1")
// does not false-positive.
func isDeclaredAPIVersion(line, api string) bool {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "apiVersion:") {
		return false
	}
	v := strings.TrimSpace(strings.TrimPrefix(s, "apiVersion:"))
	if i := strings.Index(v, "#"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return strings.Trim(v, `"'`) == api
}

func underAnyPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

type droppedAPIHit struct{ loc, api, since, fix string }

// scanDroppedAPIVersions walks git-tracked YAML under scannedManifestTrees, rooted
// at root (""/"." = cwd), for any apiVersion in droppedAPIs. inRepo is false when
// root is not a git repo — the only legitimate skip; examined counts the files
// actually read so a caller can refuse to pass on an empty corpus.
func scanDroppedAPIVersions(root string) (hits []droppedAPIHit, examined int, inRepo bool, err error) {
	out, err := gitOutput(root, "ls-files")
	if err != nil {
		if _, repoErr := gitOutput(root, "rev-parse", "--git-dir"); repoErr != nil {
			return nil, 0, false, nil
		}
		return nil, 0, true, fmt.Errorf("dropped-apiVersion scan: this IS a git repo but `git ls-files` failed, "+
			"so nothing was scanned — refusing to report clean: %w", err)
	}
	for _, f := range strings.Split(out, "\n") {
		f = strings.TrimSpace(f)
		if f == "" || !(strings.HasSuffix(f, ".yaml") || strings.HasSuffix(f, ".yml")) {
			continue
		}
		if !underAnyPrefix(f, scannedManifestTrees) {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(root, f))
		if readErr != nil {
			continue // race with a delete / unreadable — not this check's concern
		}
		examined++
		for i, ln := range strings.Split(string(data), "\n") {
			for _, d := range droppedAPIs {
				if isDeclaredAPIVersion(ln, d.apiVersion) {
					hits = append(hits, droppedAPIHit{fmt.Sprintf("%s:%d", f, i+1), d.apiVersion, d.since, d.fix})
				}
			}
		}
	}
	return hits, examined, true, nil
}

// reportDroppedAPIVersions prints hits and returns the gate error, or nil if clean.
func reportDroppedAPIVersions(hits []droppedAPIHit) error {
	if len(hits) == 0 {
		return nil
	}
	fmt.Fprintf(os.Stderr, "dropped apiVersion(s) in %d location(s):\n", len(hits))
	for _, h := range hits {
		// ::error file=…:: so each lands as a PR annotation rather than being buried
		// in log output, matching the other manifest guards.
		fmt.Fprintf(os.Stderr, "::error file=%s::%s no longer served since %s; %s\n",
			strings.SplitN(h.loc, ":", 2)[0], h.api, h.since, h.fix)
		fmt.Fprintf(os.Stderr, "  • %s — %s no longer served since %s; %s\n", h.loc, h.api, h.since, h.fix)
	}
	return fmt.Errorf("manifest(s) declare an apiVersion the cluster no longer serves — they fail to " +
		"apply (Argo SyncFailed at deploy). Migrate them by hand: `llz upgrade` does not rewrite " +
		"operator-owned files, and no CRD-aware gate covers the shared platform-apl/ tree")
}

// stepDroppedAPIVersions fails when a manifest under scannedManifestTrees declares
// an apiVersion the cluster no longer serves (see droppedAPIs). Same git-tracked
// scan shape as stepConflictMarkers, narrowed to those trees' YAML.
//
// Tolerates an empty corpus: an instance repo legitimately has no platform-apl/ and
// may have no custom YAML at all. The CI face (`llz ci dropped-apiversions`) runs in
// the template, where empty means the trees moved — and refuses to pass on it.
func stepDroppedAPIVersions(_ globalOpts) error {
	hits, _, inRepo, err := scanDroppedAPIVersions("")
	if err != nil {
		return err
	}
	if !inRepo {
		fmt.Fprintln(os.Stderr, "  skip: not a git repo (dropped-apiVersion scan)")
		return nil
	}
	return reportDroppedAPIVersions(hits)
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
		stepConflictMarkers, stepDroppedAPIVersions, stepVendoredFresh, stepUpgradeChurnGuard, stepRenderFresh,
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
		{"conflict-markers", "fail on committed merge-conflict markers", stepConflictMarkers},
		{"vendored-fresh", "fail when a vendored .github/ file drifts from the template", stepVendoredFresh},
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
