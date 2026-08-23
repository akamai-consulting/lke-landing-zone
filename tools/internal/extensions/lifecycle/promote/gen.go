package promote

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
// the LOCAL ./.github/workflows/llz-terraform.yml — no org, no @<ref>. The only
// caller boilerplate left (instance_repo) is NOT regenerated from the ranks — it
// is PRESERVED from the file already on disk (or, on a fresh instance, lifted
// from the sibling terraform.yml caller stub, or finally read from
// .copier-answers.yml). A legacy instance whose stubs still carry a cross-repo
// `uses:@<ref>` keeps that form verbatim (it has no vendored body to point at),
// so a template-version bump never shows up as pipeline "drift"; only a
// promotion_rank change does, which is exactly what `llz env pipeline --check`
// gates in CI. Stages carry no template-ref: the ref is read at runtime from the
// instance's own pin (see pinnedTemplateRef), so promote.yml no longer churns on
// every upgrade.

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

// preflightJob is the job id every generated stage chains from. NOT named
// "preflight": job ids in this file are deployment names, and a deployment may
// legitimately be called that — a collision would emit a duplicate YAML key and
// GitHub would reject the whole workflow with an error naming neither cause.
//
// The `llz-` prefix makes the collision unlikely, NOT impossible: EnvNameRe
// accepts `llz-preflight` perfectly well. validate.ReservedEnvNames is what holds
// the name; keep the two in step (a coupling test does).
const preflightJob = "llz-preflight"

// checkoutAction is the pinned actions/checkout the preflight job uses — the same
// pin the vendored workflows carry. Restated here because this file is GENERATED
// Go, not a copier template, so it cannot read the YAML's pin; keep the two in
// step when the action is bumped (`make version-pins-check` does not cover
// third-party action SHAs).
const checkoutAction = "actions/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0 # v7.0.0"

// promoCaller is the caller-stub boilerplate shared by every promote stage: which
// reusable workflow to call and the instance repo. Reused verbatim across stages
// so promote.yml calls exactly what terraform.yml does.
type promoCaller struct {
	uses         string // ./.github/workflows/llz-terraform.yml (legacy instances: <org>/lke-landing-zone/…@<ref>)
	instanceRepo string
}

var (
	reUsesLocal = regexp.MustCompile(`(?m)^\s*uses:\s*(\./\.github/workflows/llz-terraform\.yml)\s*$`)
	reUsesCross = regexp.MustCompile(`(?m)^\s*uses:\s*(\S+/lke-landing-zone/\.github/workflows/llz-terraform\.yml@\S+)`)
	// `[ \t]*`, NOT `\s*`, and the difference corrupted files. `\s` matches newlines,
	// so against an `instance_repo:` with an EMPTY value this skipped the line break
	// and captured the next key — rendering `instance_repo: action:`, which is not
	// valid YAML. The file then failed to parse on every subsequent run, so the gate
	// this PR is about answered "parse promote.yml: mapping values are not allowed in
	// this context" with no remedy, about a file llz itself had just written.
	// Reproduced by running `llz env pipeline` twice on a tree whose promote.yml
	// carries a `uses:` and no instance_repo.
	reInstanceErr = regexp.MustCompile(`(?m)^[ \t]*instance_repo:[ \t]*(\S+)`)
)

// callerFromWorkflow extracts the pin from an existing rendered caller stub
// (promote.yml or terraform.yml). Returns ok=false if the file is absent, does
// not carry an llz-terraform.yml `uses:` line, carries copier tokens (an
// unrendered template), or names no instance_repo. The local `./` form is
// preferred; a legacy cross-repo `uses:@<ref>` is preserved verbatim so old
// instances keep the pin they run.
//
// A `uses:` WITHOUT AN instance_repo IS NOT AN ANSWER, and accepting it as one is
// how the generator came to write a stub with an empty `instance_repo: `. That
// stub calls the reusable body with no repo — it cannot run — and it short-circuits
// the fallback chain below, so the .copier-answers.yml that could have supplied the
// value was never consulted. A hand-maintained promote.yml is exactly the shape
// that trips it, which is the population `llz env pipeline` is now the remedy for.
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
	// A local `uses:` is a literal that exists in the un-rendered template too, so
	// it no longer proves the stub is rendered — reject leftover copier tokens, and
	// an absent value along with them. Both mean the same thing here: this file
	// cannot tell us the repo, so ask the next source rather than render a stub that
	// names none.
	if c.instanceRepo == "" || strings.Contains(c.instanceRepo, "<@") {
		return promoCaller{}, false
	}
	return c, true
}

// resolveCaller finds the pin to render with. Preference order, each a fallback
// for the previous being absent/unrendered:
//  1. the existing promote.yml  — preserve what it calls.
//  2. the sibling terraform.yml — a fresh instance has this rendered already.
//  3. .copier-answers.yml (instance_repo).
func resolveCaller(d Deps, workflowsDir string) (promoCaller, error) {
	if c, ok := callerFromWorkflow(filepath.Join(workflowsDir, "promote.yml")); ok {
		return c, nil
	}
	if c, ok := callerFromWorkflow(filepath.Join(workflowsDir, "terraform.yml")); ok {
		return c, nil
	}
	repo := d.InstanceRepo()
	if repo == "" {
		return promoCaller{}, fmt.Errorf("cannot determine the caller: no rendered promote.yml/terraform.yml to copy it from, and .copier-answers.yml has no instance_repo")
	}
	return promoCaller{
		uses:         localTerraformUses, // the vendored body (ADR 0003)
		instanceRepo: repo,
	}, nil
}

// unmanagedNote says what is actually true of a tree with too few ranks to
// generate from, WITHOUT claiming a write that is not happening. The note used to
// be shared with the remedy path and read "generating the no-stage placeholder"
// on a run that generated nothing — a message describing the other branch.
func unmanagedNote(ranked, onDisk int) string {
	if onDisk == 0 {
		return fmt.Sprintf("promote.yml: %d ranked deployment(s) — need ≥2 to form a pipeline; nothing to generate yet (set promotionRank on the deployments you want to chain).", ranked)
	}
	// "none naming a deployment that does not exist" rather than "all naming declared
	// deployments": this path is reached when nothing AUTHORISES a rewrite, which is
	// not the same as everything having been checked. A stage whose `region:` is an
	// expression is left alone and never compared, so the stronger sentence was false
	// for exactly the files this branch added the leave-alone path to protect.
	return fmt.Sprintf("promote.yml: %d stage(s) on disk, none naming a deployment this instance does not have, but %d ranked deployment(s) — llz is not managing this file. Leaving it as it is; set promotionRank on the deployments you want chained to have `llz env pipeline` generate it.", onDisk, ranked)
}

// renderPlaceholderWorkflow renders the EMPTY pipeline: the preflight, and no
// stages. It is what `llz env pipeline` writes when there are fewer than two
// ranked deployments, so the generator is total over the rank count instead of
// declining at the bottom of the range.
//
// NOT byte-identical to the promote.yml instance-template/ ships, and does not
// need to be: the contract for the empty pipeline is "declares no stages", which
// is what the checker measures. That is deliberate — pinning the two together
// would force the scaffold's adopter-facing prose into a Go string literal to
// keep a fresh instance from reporting drift against itself, and buy nothing the
// stage check does not already give.
func renderPlaceholderWorkflow() string {
	return `# GENERATED by ` + "`llz env pipeline`" + ` — the EMPTY pipeline. This instance has fewer
# than two ranked deployments, so there is no chain to walk and this file declares
# no stages. Rank two deployments (` + "`promotionRank`" + ` in environments/<env>.yaml, or
# ` + "`llz env add --promotion-rank N`" + `) and re-run ` + "`llz env pipeline`" + ` to generate them.
# DO NOT EDIT BY HAND. See docs/environments-and-promotion.md.

name: Promote (no pipeline configured yet)

on:
  # Dispatchable so the workflow stays discoverable and can say what to do; the
  # preflight below fails the run rather than promoting nothing quietly.
  workflow_dispatch:

permissions:
  contents: read

concurrency:
  group: promote
  cancel-in-progress: false

jobs:
  ` + preflightJob + `:
    name: Promotion pipeline matches the spec
    runs-on: ubuntu-latest
    container:
      image: ${{ vars.TF_IMAGE }}
    permissions:
      contents: read
    timeout-minutes: 5
    steps:
      - name: Checkout the instance
        uses: ` + checkoutAction + `
      # IMAGE SKEW IS CHECKED FIRST, AND THE ORDER IS THE WHOLE POINT — the rule
      # repo-readiness records in llz-terraform.yml. A TF_IMAGE that has not been
      # re-pinned since the last upgrade is the NORMAL state right after one, and
      # that image's baked llz does not have --require-pipeline: with the steps the
      # other way round this job dies on "unknown flag" and the actionable message
      # never runs, on exactly the population it was built for. assert-image-fresh
      # has shipped for many releases, so it resolves in the old image and says the
      # useful thing. Any new verb or flag added here belongs BELOW it.
      - name: ci images match the instance's template pin
        env:
          GH_TOKEN: ${{ github.token }}
        run: llz ci assert-image-fresh
      - name: Verify there is a pipeline and every stage names a declared deployment
        run: llz env pipeline --check --require-pipeline
`
}

// renderPromoteWorkflow renders the full promote.yml body for the ordered stages.
// Pure (no I/O) so it unit-tests against a fixed caller + stage list. Caller
// guarantees len(stages) >= 2.
func renderPromoteWorkflow(c promoCaller, stages []promoStage) string {
	var b strings.Builder
	b.WriteString(`# GENERATED from each deployment's promotion_rank (cluster/<env>.tfvars) by
# ` + "`llz env add`" + ` / ` + "`llz env pipeline`" + `. DO NOT EDIT BY HAND — re-run
# ` + "`llz env pipeline`" + ` after changing a promotion_rank to regenerate, and wire
# ` + "`llz env pipeline --check`" + ` into CI — it fails both when this file drifts from
# the ranks AND when a stage names a deployment the spec no longer declares.
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
  # ── Preflight ────────────────────────────────────────────────────────────────
  # The first stage needs: this, so the whole chain is gated on the pipeline still
  # agreeing with the spec. Without it a dispatch had NO gate: the PR-time twin
  # (promote-pipeline-drift in llz-terraform.yml) only runs on pull_request, so a
  # promote.yml naming a deployment that had been renamed or never created got as
  # far as three parallel stages each dying inside ` + "`llz render`" + ` on "env not in
  # spec" — one root cause, three unrelated-looking failures, ~20s apiece.
  ` + preflightJob + `:
    name: Promotion pipeline matches the spec
    runs-on: ubuntu-latest
    container:
      image: ${{ vars.TF_IMAGE }}
    permissions:
      contents: read
    timeout-minutes: 5
    steps:
      - name: Checkout the instance
        uses: ` + checkoutAction + `
      # IMAGE SKEW IS CHECKED FIRST, AND THE ORDER IS THE WHOLE POINT — the rule
      # repo-readiness records in llz-terraform.yml. A TF_IMAGE that has not been
      # re-pinned since the last upgrade is the NORMAL state right after one, and
      # that image's baked llz does not have --require-pipeline: with the steps the
      # other way round this job dies on "unknown flag" and the actionable message
      # never runs, on exactly the population it was built for. assert-image-fresh
      # has shipped for many releases, so it resolves in the old image and says the
      # useful thing. Any new verb or flag added here belongs BELOW it.
      - name: ci images match the instance's template pin
        env:
          GH_TOKEN: ${{ github.token }}
        run: llz ci assert-image-fresh
      - name: Verify there is a pipeline and every stage names a declared deployment
        run: llz env pipeline --check --require-pipeline

`)
	for i, s := range stages {
		b.WriteString(fmt.Sprintf("  %s:\n", s.name))
		b.WriteString(fmt.Sprintf("    name: Promote → %s (rank %d)\n", s.name, s.rank))
		if i > 0 {
			// `needs:` the previous stage — the on-green gate.
			b.WriteString(fmt.Sprintf("    needs: %s\n", stages[i-1].name))
		} else {
			// The entry stage chains from the preflight instead of from a prior
			// stage, so "no needs:" never means "no gate".
			b.WriteString(fmt.Sprintf("    needs: %s\n", preflightJob))
		}
		b.WriteString(fmt.Sprintf("    uses: %s\n", c.uses))
		b.WriteString("    with:\n")
		b.WriteString(fmt.Sprintf("      instance_repo: %s\n", c.instanceRepo))
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
// Plan is what the caller must do to bring promote.yml in line with the ranks.
// Content is empty when nothing needs writing.
type Plan struct {
	Path    string   // .github/workflows/promote.yml, or "" on a template-repo checkout
	Content string   // rendered workflow; "" means "leave the file alone"
	Changed bool     // the file is absent or differs
	Order   []string // stage names, for the caller's progress line
	Note    string   // a human note when nothing was generated and the reason is not obvious

	// Stages is every stage the ON-DISK promote.yml declares, undeclared ones
	// included. What it answers that Order does not: Order is what the RANKS say
	// the pipeline should be, and this is what the file being dispatched actually
	// is. `llz env pipeline --check --require-pipeline` needs the second.
	Stages []StageRef
	// Undeclared holds the stages the ON-DISK promote.yml declares that name a
	// deployment this instance does not have. Populated INDEPENDENTLY of Changed
	// and of the rank count — it is the only field that says anything about the
	// tree with 0 or 1 ranked deployments, which is the state every fresh instance
	// is in and the state the dev→staging→prod failure happened in.
	Undeclared []StageRef
	// Unresolved holds the stages whose `region:` is a GitHub expression. SEPARATE
	// FROM Undeclared because it is not a failure: there is no name to compare, so
	// the only honest report is "unverified", and calling it undeclared once made
	// `llz env pipeline` delete a working operator-maintained pipeline. Surfaced
	// through Advisories(), which is also what stops the passing message claiming
	// every stage was checked.
	Unresolved []StageRef
	// Declared is every deployment the instance has, for the failure message.
	Declared []string
}

// PlanWorkflow renders the promotion workflow and reports whether it differs from
// what is on disk. IT WRITES NOTHING, and that is deliberate.
//
// The declaration for this extension is `transition:promoted[read-repo]`, and an
// os.WriteFile in this package would make that declaration FALSE. The model has no
// `write-repo` grant: `own-paths` is the nearest-looking one and is the wrong one —
// per the catalog's Decision 1 it means "copier must not render these bytes", a
// FENCE rather than a write permit, and Validate() rejects it here anyway because
// it is only meaningful at `scaffolded` or `upgraded`.
//
// So the file split follows the declaration rather than the other way round:
// rendering lives here, the write lives in cmd/llz. This is the SAME resolution
// `guard-docs` reached for `llz ci gen-toc`, and the catalog records the reasoning
// — two independent cases is enough to say the vocabulary has a hole, and not
// enough to know its shape, so nothing is invented. This is the THIRD case and it
// resolved the same way, which is evidence the split is a real answer and not a
// workaround. TestPackageContainsNoWritePath keeps it honest.
func PlanWorkflow(d Deps, tfDir, relPrefix string) (Plan, error) {
	path, generate := promoteWorkflowPath(relPrefix)
	if !generate {
		return Plan{}, nil // template-repo checkout — nothing to generate
	}

	// The deployment set and the on-disk stages are read FIRST, and the comparison
	// between them happens regardless of how many ranks exist. Everything below
	// this point is about regenerating from ranks; the check above is about whether
	// what is already there can run at all, and those are not the same question.
	declared, err := DeploymentNames(d, tfDir)
	if err != nil {
		return Plan{}, err
	}
	got, readErr := os.ReadFile(path)
	var onDisk, undeclared, unresolved []StageRef
	switch {
	case readErr == nil:
		onDisk, err = workflowStages(got)
		if err != nil {
			return Plan{}, fmt.Errorf("%s: %w", path, err)
		}
		undeclared, unresolved = undeclaredStages(onDisk, declared)
	case !os.IsNotExist(readErr):
		// ABSENT AND UNREADABLE ARE NOT THE SAME ANSWER. No file is a real state —
		// nothing has been generated yet, and no claim is made. A file we cannot
		// read is "could not tell", and treating it as "no stages" would hand back
		// the exact abstention-as-agreement this gate exists to remove: a chmod 000
		// (or a half-written file) would turn a failing promote.yml green.
		return Plan{}, fmt.Errorf("%s: %w", path, readErr)
	}

	stages, err := ReadPromotion(d, tfDir)
	if err != nil {
		return Plan{}, err
	}
	if len(stages) < 2 {
		// THE PLACEHOLDER IS A REMEDY, NOT A NORMALISATION, and the distinction is
		// the difference between fixing a broken file and deleting a working one.
		//
		// An earlier cut rendered it for EVERY tree with <2 ranks, which silently
		// destroyed a perfectly good promote.yml: an instance declaring dev and prod
		// with valid stages but no promotionRank set — the state the old shipped
		// example put people in — had `--check` demand a regeneration that then
		// deleted both stages. `llz env add` did the same overwrite with no prompt.
		//
		// Unranked is not broken. A hand-maintained chain over declared deployments
		// applies exactly what it says; the ranks simply are not managing it. Only
		// a stage naming a deployment that DOES NOT EXIST is unrunnable, and that is
		// the only thing worth overwriting a file to fix.
		// Hoisted out of the `if` so the note below can report the count that actually
		// authorised the write. It used to print len(undeclared), which is a different
		// and larger number: undeclared also holds the stages llz merely could not
		// resolve, and none of those are a reason to overwrite anything.
		missing := namesMissingDeployment(undeclared)
		if len(missing) == 0 {
			return Plan{Path: path, Stages: onDisk, Undeclared: undeclared, Unresolved: unresolved, Declared: declared,
				Note: unmanagedNote(len(stages), len(onDisk))}, nil
		}
		// Undeclared stages AND too few ranks to regenerate a real chain from — the
		// gsap-apl shape. Abstaining here left it with no route back to green:
		// `--check` failed, and the fix it printed ("rank two deployments and
		// regenerate") is unreachable when you only have one. The empty pipeline is
		// a state the generator can render, so it renders it.
		return Plan{Path: path, Content: renderPlaceholderWorkflow(), Changed: true,
			Stages: onDisk, Undeclared: undeclared, Unresolved: unresolved, Declared: declared,
			Note: fmt.Sprintf("promote.yml: %d stage(s) name deployments this instance does not declare, and %d ranked deployment(s) is too few to regenerate a chain — replacing it with the no-stage placeholder.",
				len(missing), len(stages))}, nil
	}

	caller, err := resolveCaller(d, filepath.Dir(path))
	if err != nil {
		return Plan{}, err
	}
	want := renderPromoteWorkflow(caller, stages)

	if string(got) == want {
		return Plan{Path: path, Stages: onDisk, Undeclared: undeclared, Unresolved: unresolved, Declared: declared}, nil
	}
	order := make([]string, len(stages))
	for i, st := range stages {
		order[i] = st.name
	}
	return Plan{Path: path, Content: want, Changed: true, Order: order, Stages: onDisk, Undeclared: undeclared, Unresolved: unresolved, Declared: declared}, nil
}
