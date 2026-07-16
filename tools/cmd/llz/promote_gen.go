package main

// promote_gen.go renders the native code-promotion workflow
// (.github/workflows/promote.yml) from the per-deployment `promotion_rank`
// declared in cluster tfvars (see promotion.go). This is "option 2" from
// docs/environments-and-promotion.md: promotion_rank stays the single source of
// truth, and `llz env add` (plus `llz env pipeline`) regenerates a STATIC
// `needs:`-chained workflow so the runtime is 100% GitHub-native — `needs:` is
// the on-green gate, the infra-<stage> Environment protection rules are the
// approval/soak gate, and GitHub's "Re-run failed jobs" is the resume. There is
// no self-dispatch loop to reinvent.
//
// The reusable body is vendored into the instance (ADR 0003), so the `uses:` is
// the LOCAL ./.github/workflows/llz-terraform.yml — no org, no @<ref>. The pin
// that remains (instance_repo, template-ref, and the renovate depName annotated
// on template-ref) is NOT regenerated from the ranks — it is PRESERVED from the
// file already on disk (or, on a fresh instance, lifted from the sibling
// terraform.yml caller stub, or finally derived from .copier-answers.yml +
// .template-version). A legacy instance whose stubs still carry a cross-repo
// `uses:@<ref>` keeps that form verbatim (it has no vendored body to point at),
// so a template-version bump never shows up as pipeline "drift"; only a
// promotion_rank change does, which is exactly what `llz env pipeline --check`
// gates in CI.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// localTerraformUses is the vendored-body reference every instance rendered from
// ADR 0003 onward carries: same-repo, so `secrets: inherit` resolves and nothing
// is fetched cross-repo at runtime.
const localTerraformUses = "./.github/workflows/llz-terraform.yml"

// promoCaller is the caller-stub boilerplate shared by every promote stage: which
// reusable workflow to call, the instance repo, the template-ref input, and the
// renovate depName annotated on it. Reused verbatim across stages so promote.yml
// calls exactly what terraform.yml does.
type promoCaller struct {
	uses         string // ./.github/workflows/llz-terraform.yml (legacy instances: <org>/lke-landing-zone/…@<ref>)
	instanceRepo string
	templateRef  string
	depName      string // <org>/lke-landing-zone — the `# renovate:` depName for the template-ref pin
}

var (
	reUsesLocal   = regexp.MustCompile(`(?m)^\s*uses:\s*(\./\.github/workflows/llz-terraform\.yml)\s*$`)
	reUsesCross   = regexp.MustCompile(`(?m)^\s*uses:\s*(\S+/lke-landing-zone/\.github/workflows/llz-terraform\.yml@\S+)`)
	reInstanceErr = regexp.MustCompile(`(?m)^\s*instance_repo:\s*(\S+)`)
	reTemplateRef = regexp.MustCompile(`(?m)^\s*template-ref:\s*(\S+)`)
	reDepName     = regexp.MustCompile(`(?m)^\s*#\s*renovate:\s*datasource=github-tags\s+depName=(\S+)`)
)

// callerFromWorkflow extracts the pin from an existing rendered caller stub
// (promote.yml or terraform.yml). Returns ok=false if the file is absent, does
// not carry an llz-terraform.yml `uses:` line, or still carries copier tokens
// (an unrendered template). The local `./` form is preferred; a legacy cross-repo
// `uses:@<ref>` is preserved verbatim so old instances keep the pin they run.
func callerFromWorkflow(path string) (promoCaller, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return promoCaller{}, false
	}
	s := string(b)
	var c promoCaller
	if m := reUsesLocal.FindStringSubmatch(s); m != nil {
		c.uses = m[1]
	} else if m := reUsesCross.FindStringSubmatch(s); m != nil && !strings.Contains(m[1], "<@") {
		c.uses = m[1]
	} else {
		return promoCaller{}, false // no concrete uses: line
	}
	if m := reInstanceErr.FindStringSubmatch(s); m != nil {
		c.instanceRepo = m[1]
	}
	if m := reTemplateRef.FindStringSubmatch(s); m != nil {
		c.templateRef = m[1]
	}
	if m := reDepName.FindStringSubmatch(s); m != nil {
		c.depName = m[1]
	} else if c.uses != localTerraformUses {
		c.depName = depNameFromUses(c.uses) // legacy pin carries the org in uses:
	}
	// A local `uses:` is a literal that exists in the un-rendered template too, so
	// it no longer proves the stub is rendered — reject leftover copier tokens.
	if strings.Contains(c.instanceRepo, "<@") || strings.Contains(c.templateRef, "<@") || strings.Contains(c.depName, "<@") {
		return promoCaller{}, false
	}
	return c, true
}

// resolveCaller finds the pin to render with. Preference order, each a fallback
// for the previous being absent/unrendered:
//  1. the existing promote.yml  — preserve its pin (Renovate may have bumped it).
//  2. the sibling terraform.yml — a fresh instance has this rendered already.
//  3. .copier-answers.yml (upstream_org + instance_repo) + .template-version ref.
func resolveCaller(workflowsDir string) (promoCaller, error) {
	if c, ok := callerFromWorkflow(filepath.Join(workflowsDir, "promote.yml")); ok {
		return c, nil
	}
	if c, ok := callerFromWorkflow(filepath.Join(workflowsDir, "terraform.yml")); ok {
		return c, nil
	}
	a, _ := readAnswers(".")
	ref := templateRefFromStamp()
	if a == nil || a.UpstreamOrg == "" || a.InstanceRepo == "" || ref == "" {
		return promoCaller{}, fmt.Errorf("cannot determine the caller pin: no rendered promote.yml/terraform.yml to copy it from, and .copier-answers.yml/.template-version are incomplete")
	}
	return promoCaller{
		uses:         localTerraformUses, // the vendored body (ADR 0003)
		instanceRepo: a.InstanceRepo,
		templateRef:  ref,
		depName:      a.UpstreamOrg + "/lke-landing-zone",
	}, nil
}

// templateRefFromStamp reads the template_ref out of .template-version (best
// effort; "" if absent/malformed).
func templateRefFromStamp() string {
	b, err := os.ReadFile(".template-version")
	if err != nil {
		return ""
	}
	m := regexp.MustCompile(`"template_ref"\s*:\s*"([^"]+)"`).FindSubmatch(b)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// renderPromoteWorkflow renders the full promote.yml body for the ordered stages.
// Pure (no I/O) so it unit-tests against a fixed caller + stage list. Caller
// guarantees len(stages) >= 2.
func renderPromoteWorkflow(c promoCaller, stages []promoStage) string {
	var b strings.Builder
	b.WriteString(`# GENERATED from each deployment's promotion_rank (cluster/<env>.tfvars) by
# ` + "`llz env add`" + ` / ` + "`llz env pipeline`" + `. DO NOT EDIT BY HAND — re-run
# ` + "`llz env pipeline`" + ` after changing a promotion_rank to regenerate, and wire
# ` + "`llz env pipeline --check`" + ` into CI to fail when this file drifts from the ranks.
#
# Native code-promotion pipeline (docs/environments-and-promotion.md): a static
# needs:-chain over the ranked deployments. Each stage calls the same reusable
# llz-terraform.yml apply path terraform.yml uses; ` + "`needs:`" + ` is the on-green gate
# (a stage starts only once the prior stage applied AND converged) and the
# infra-<stage> Environment protection rules are the approval/soak gate. Resume a
# failed run with GitHub's built-in "Re-run failed jobs".

name: Promote (`)
	for i, s := range stages {
		if i > 0 {
			b.WriteString(" → ")
		}
		b.WriteString(s.name)
	}
	b.WriteString(`)

on:
  workflow_dispatch:
    inputs:
      module:
        description: 'How much of each stage to (re)apply for this promotion'
        required: false
        type: choice
        default: all
        options:
          - all
          - cluster
          - object-storage

permissions:
  contents: read

# One promotion in flight at a time; never cancel an in-progress rollout.
concurrency:
  group: promote
  cancel-in-progress: false

jobs:
`)
	for i, s := range stages {
		b.WriteString(fmt.Sprintf("  %s:\n", s.name))
		b.WriteString(fmt.Sprintf("    name: Promote → %s (rank %d)\n", s.name, s.rank))
		if i > 0 {
			// `needs:` the previous stage — the on-green gate.
			b.WriteString(fmt.Sprintf("    needs: %s\n", stages[i-1].name))
		}
		b.WriteString(fmt.Sprintf("    uses: %s\n", c.uses))
		b.WriteString("    with:\n")
		b.WriteString(fmt.Sprintf("      instance_repo: %s\n", c.instanceRepo))
		if c.depName != "" {
			b.WriteString("      # renovate: datasource=github-tags depName=" + c.depName + "\n")
		}
		b.WriteString(fmt.Sprintf("      template-ref: %s\n", c.templateRef))
		b.WriteString("      action: apply\n")
		b.WriteString("      module: ${{ inputs.module || 'all' }}\n")
		b.WriteString(fmt.Sprintf("      region: %s\n", s.name))
		b.WriteString("    secrets: inherit\n")
		if i < len(stages)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// depNameFromUses turns a LEGACY cross-repo `uses:` value into the <org>/<repo>
// slug Renovate tracks (mirrors the `# renovate:` comment terraform.yml carries on
// template-ref). Only meaningful for the cross-repo form — a local `./` uses
// carries no org; its depName comes from the annotation or .copier-answers.yml.
func depNameFromUses(uses string) string {
	// <org>/lke-landing-zone/.github/workflows/llz-terraform.yml@<ref>
	path := uses
	if i := strings.Index(path, "@"); i >= 0 {
		path = path[:i]
	}
	if i := strings.Index(path, "/.github/"); i >= 0 {
		path = path[:i]
	}
	return path
}

// promoteWorkflowPath returns where promote.yml lives for the detected layout, and
// whether generation applies. Generation is for a RENDERED INSTANCE only
// (relPrefix == ""); a template-repo checkout keeps the hand-maintained
// instance-template/.github/workflows/promote.yml with copier tokens, which has no
// ranked tfvars to generate from.
func promoteWorkflowPath(relPrefix string) (path string, generate bool) {
	if relPrefix != "" {
		return filepath.Join(strings.TrimSuffix(relPrefix, "/"), ".github", "workflows", "promote.yml"), false
	}
	return filepath.Join(".github", "workflows", "promote.yml"), true
}

// syncPromoteWorkflow reconciles promote.yml with the current promotion_rank set.
//   - check=false: write the file if it differs (or print a skip note). Best-effort
//     from `llz env add` — a failure warns, it does not abort the scaffold.
//   - check=true: write nothing; return changed=true if the on-disk file differs
//     from what the ranks would render (the CI drift gate).
//
// Returns changed=true when the file was (or would be) rewritten.
func syncPromoteWorkflow(tfDir, relPrefix string, check bool) (changed bool, err error) {
	path, generate := promoteWorkflowPath(relPrefix)
	if !generate {
		return false, nil // template-repo checkout — nothing to generate
	}

	stages, err := readPromotion(tfDir)
	if err != nil {
		return false, err
	}
	if len(stages) < 2 {
		// A pipeline needs at least two stages. Leave any existing file untouched
		// (an operator may be mid-setup) and say so rather than writing a stub.
		if !check {
			fmt.Printf("promote.yml: %d ranked deployment(s) — need ≥2 to form a pipeline; not generated yet (set promotion_rank on the stages you want to chain).\n", len(stages))
		}
		return false, nil
	}

	caller, err := resolveCaller(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	want := renderPromoteWorkflow(caller, stages)

	got, _ := os.ReadFile(path)
	if string(got) == want {
		return false, nil
	}
	if check {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		return false, err
	}
	order := make([]string, len(stages))
	for i, s := range stages {
		order[i] = s.name
	}
	fmt.Printf("promote.yml: regenerated pipeline %s\n", strings.Join(order, " → "))
	return true, nil
}
