package promote

// stages_test.go — the gate for the failure that started this: a promote.yml
// chaining dev → staging → prod, dispatched on an instance whose spec declared
// only `prod`, while `llz env pipeline --check` reported the file "in sync".
//
// TestPlanFlagsStagesTheSpecDoesNotDeclare reconstructs that exact tree. It fails
// against the code as it was.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/manifest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/validate"
)

// specDeps returns Deps whose LoadSpec reports a real LandingZone with the named
// deployments at the given ranks — the SPEC path, which is what every instance
// scaffolded since the spec landed actually takes. testDeps() covers the legacy
// tfvars path; both need to agree, so both are exercised.
func specDeps(ranks map[string]int) Deps {
	lz := &clusterspec.LandingZone{}
	lz.Spec.Environments = map[string]clusterspec.Environment{}
	for name, rank := range ranks {
		var e clusterspec.Environment
		e.Cluster.PromotionRank = rank
		lz.Spec.Environments[name] = e
	}
	return Deps{
		Layout:          func() (string, string, string) { return "tf", "platform-apl", "" },
		ListDeployments: listDeploymentsFromDisk,
		LoadSpec:        func() (*clusterspec.LandingZone, bool, error) { return lz, true, nil },
		InstanceRepo:    func() string { return "myorg/my-instance" },
	}
}

// threeStageWorkflow is the shape the template used to ship and gsap-apl carried:
// three live stages, no preflight.
const threeStageWorkflow = `name: Promote (dev → staging → prod)
on:
  workflow_dispatch:
jobs:
  dev:
    uses: ./.github/workflows/llz-terraform.yml
    with:
      instance_repo: myorg/my-instance
      action: apply
      region: dev
    secrets: inherit
  staging:
    needs: dev
    uses: ./.github/workflows/llz-terraform.yml
    with:
      instance_repo: myorg/my-instance
      action: apply
      region: staging
    secrets: inherit
  prod:
    needs: staging
    uses: ./.github/workflows/llz-terraform.yml
    with:
      instance_repo: myorg/my-instance
      action: apply
      region: prod
    secrets: inherit
`

func TestWorkflowStagesReadsOnlyStages(t *testing.T) {
	// A preflight job (no llz-terraform uses:) is not a stage and must not be
	// checked as one — otherwise the job this change ADDS would fail the gate it
	// was added to enforce.
	body := `jobs:
  llz-preflight:
    runs-on: ubuntu-latest
    steps:
      - run: llz env pipeline --check
  dev:
    uses: ./.github/workflows/llz-terraform.yml
    with:
      region: dev
  prod:
    uses: akamai-consulting/lke-landing-zone/.github/workflows/llz-terraform.yml@v1.2.3
    with:
      region: prod
`
	got, err := workflowStages([]byte(body))
	if err != nil {
		t.Fatalf("workflowStages: %v", err)
	}
	// Both the vendored-local and the legacy cross-repo form count as stages; the
	// preflight does not.
	want := []StageRef{{Job: "dev", Env: "dev"}, {Job: "prod", Env: "prod"}}
	if len(got) != len(want) {
		t.Fatalf("stages = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Job != want[i].Job || got[i].Env != want[i].Env {
			t.Errorf("stage %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A file that cannot be parsed is an ERROR, not "no stages". Reporting an
// unreadable pipeline as empty would pass the gate on precisely the tree where
// nobody can tell what the pipeline does.
func TestWorkflowStagesFailsClosedOnMalformedYAML(t *testing.T) {
	if _, err := workflowStages([]byte("jobs:\n  dev:\n   uses: x\n  \tbad indent\n")); err == nil {
		t.Error("malformed promote.yml must be an error, not an empty stage list")
	}
}

func TestUndeclaredStages(t *testing.T) {
	stages := []StageRef{{Job: "dev", Env: "dev"}, {Job: "prod", Env: "prod"}, {Job: "broken", Env: ""}}

	bad, unresolved := undeclaredStages(stages, []string{"dev", "prod"})
	if len(bad) != 1 || bad[0].Job != "broken" {
		t.Errorf("a stage with no region: must be flagged; got %+v", bad)
	}
	if len(unresolved) != 0 {
		t.Errorf("no stage here is an expression; got %+v", unresolved)
	}

	bad, _ = undeclaredStages(stages[:2], []string{"prod"})
	if len(bad) != 1 || bad[0].Env != "dev" {
		t.Errorf("the undeclared deployment must be flagged; got %+v", bad)
	}

	// Vacuity: zero declared deployments is not a pass. A pipeline over nothing
	// applies nothing, and "we could not find any deployments" is what a broken
	// checkout looks like too.
	if bad, _ := undeclaredStages(stages[:2], nil); len(bad) != 2 {
		t.Errorf("stages with no declared deployments at all must all be flagged; got %+v", bad)
	}

	// A file with no stages makes no claim — the un-configured instance the
	// template ships. Nothing to falsify, so nothing is reported.
	if bad, _ := undeclaredStages(nil, nil); len(bad) != 0 {
		t.Errorf("a stage-less promote.yml must not be flagged; got %+v", bad)
	}
}

// THE REGRESSION. One declared deployment, zero ranks, and a promote.yml naming
// three. Before this change PlanWorkflow returned early on len(stages) < 2 and the
// CLI printed "promote.yml is in sync with the promotion_rank ordering."
func TestPlanFlagsStagesTheSpecDoesNotDeclare(t *testing.T) {
	root := t.TempDir()
	chdir(t, root)
	if err := os.MkdirAll(filepath.Join(".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(".github", "workflows", "promote.yml"), threeStageWorkflow)

	plan, err := PlanWorkflow(specDeps(map[string]int{"prod": 0}), "tf", "")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	// The stale stages ARE regenerable — to the empty pipeline. Abstaining here is
	// what left this instance shape with no route back to green.
	if !plan.Changed {
		t.Error("stale stages with <2 ranks must be regenerable to the placeholder")
	}
	if len(plan.Undeclared) != 2 {
		t.Fatalf("want dev+staging flagged as undeclared, got %+v", plan.Undeclared)
	}
	err = plan.UndeclaredErr()
	if err == nil {
		t.Fatal("UndeclaredErr must be non-nil when stages are undeclared")
	}
	msg := err.Error()
	// The message has to carry BOTH halves: the names that are wrong, and the
	// names that exist. The absent name alone never reveals the present one.
	for _, want := range []string{"dev", "staging", "prod"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message must name %q:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "declared deployments: prod") {
		t.Errorf("message must list what IS declared:\n%s", msg)
	}
}

// The same tree via the legacy tfvars path, so the two sources of deployment
// names cannot diverge in what the gate sees.
func TestPlanFlagsUndeclaredStagesOnTheTfvarsPath(t *testing.T) {
	root := t.TempDir()
	chdir(t, root)
	writeCluster(t, "tf", map[string]string{"prod.tfvars": "region = \"us-ord\"\n"})
	if err := os.MkdirAll(filepath.Join(".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(".github", "workflows", "promote.yml"), threeStageWorkflow)

	plan, err := PlanWorkflow(testDeps(), "tf", "")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Undeclared) != 2 {
		t.Errorf("tfvars path must flag dev+staging too, got %+v", plan.Undeclared)
	}
}

// An UNRANKED deployment is still a declared one. Reading the deployment set from
// PromotionRanks instead of DeploymentNames would reject this valid file, and the
// tfvars path is where the two differ (it only carries names with a rank line).
func TestUnrankedDeploymentsCountAsDeclared(t *testing.T) {
	root := t.TempDir()
	chdir(t, root)
	writeCluster(t, "tf", map[string]string{
		"dev.tfvars":     "promotion_rank = 1\n",
		"staging.tfvars": "promotion_rank = 2\n",
		"prod.tfvars":    "region = \"us-ord\"\n", // declared, NO rank
	})
	if err := os.MkdirAll(filepath.Join(".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(".github", "workflows", "promote.yml"), threeStageWorkflow)

	plan, err := PlanWorkflow(testDeps(), "tf", "")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Undeclared) != 0 {
		t.Errorf("an unranked but declared deployment must not be flagged: %+v", plan.Undeclared)
	}
}

// Coupling test across the generate/check boundary: feed the GENERATOR's real
// output to the CHECKER's real predicate. If the two ever disagree about what a
// stage looks like, `llz env pipeline` would write a file that `llz env pipeline
// --check` immediately rejects — the pipeline equivalent of the reaper losing the
// relabeler's prefix.
func TestGeneratedWorkflowPassesItsOwnCheck(t *testing.T) {
	out := renderPromoteWorkflow(testCaller(), []promoStage{{name: "dev", rank: 1}, {name: "prod", rank: 2}})

	stages, err := workflowStages([]byte(out))
	if err != nil {
		t.Fatalf("the generator emitted a promote.yml the checker cannot parse: %v", err)
	}
	if len(stages) != 2 {
		t.Fatalf("checker found %d stages in a 2-stage render: %+v", len(stages), stages)
	}
	if bad, _ := undeclaredStages(stages, []string{"dev", "prod"}); len(bad) != 0 {
		t.Errorf("generated stages rejected by the checker: %+v", bad)
	}
	// The preflight must be a job the checker skips, and the entry stage must chain
	// from it — "no needs:" on stage 1 is what left the dispatch path ungated.
	if !strings.Contains(out, "\n  "+preflightJob+":\n") {
		t.Errorf("generated workflow has no %s job:\n%s", preflightJob, out)
	}
	if !strings.Contains(out, "    needs: "+preflightJob+"\n") {
		t.Errorf("entry stage must `needs: %s`:\n%s", preflightJob, out)
	}
}

// The delivered promote.yml is checked with the REAL predicate, not by eye. It
// shipped a live dev → staging → prod chain for a long time; this is what stops it
// coming back, and it is a coupling test rather than a string match because the
// thing that made the example dangerous was that it was RUNNABLE, which is exactly
// what workflowStages measures.
func TestShippedTemplateDeclaresNoStages(t *testing.T) {
	path := filepath.Join(scaffoldDir(t), ".github", "workflows", "promote.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	stages, err := workflowStages(body)
	if err != nil {
		t.Fatalf("the shipped promote.yml does not parse: %v", err)
	}
	if len(stages) != 0 {
		t.Errorf("instance-template/.github/workflows/promote.yml ships %d live stage(s) %+v — "+
			"a fresh instance declares none of them, so dispatching it fails N jobs deep in `llz render`. "+
			"The example belongs in docs/environments-and-promotion.md, where it cannot be run.", len(stages), stages)
	}
}

// The two contexts must disagree, and this is the assertion that they do. A
// stage-less promote.yml is a NORMAL state on a pull request (every instance is in
// it until two deployments are ranked) and a FAILURE at dispatch (the operator
// pressed Run workflow on a promotion that promotes nothing). One predicate for
// both would have to pick one, and picking "pass" is how the original bug shipped.
func TestNoPipelineErrOnlyFiresWhenAskedFor(t *testing.T) {
	none := Plan{Declared: []string{"prod"}}
	if err := none.UndeclaredErr(); err != nil {
		t.Errorf("a stage-less promote.yml must not fail the PR gate: %v", err)
	}
	err := none.NoPipelineErr()
	if err == nil {
		t.Fatal("a stage-less promote.yml must fail --require-pipeline")
	}
	if !strings.Contains(err.Error(), "declared deployments: prod") {
		t.Errorf("message must name the deployments that DO exist:\n%s", err)
	}
	// One stage is still not a pipeline — a chain needs something to chain to.
	one := Plan{Stages: []StageRef{{Job: "prod", Env: "prod", Action: "apply"}}, Declared: []string{"prod"}}
	if one.NoPipelineErr() == nil {
		t.Error("a single-stage promote.yml is not a pipeline")
	}
	two := Plan{Stages: []StageRef{
		{Job: "dev", Env: "dev", Action: "apply"},
		{Job: "prod", Env: "prod", Action: "apply", Needs: []string{"dev"}},
	}}
	if err := two.NoPipelineErr(); err != nil {
		t.Errorf("two stages IS a pipeline: %v", err)
	}

	// COUNTED OVER THE APPLIES. A plan-only preview job calls the same reusable body,
	// so it read as a stage and "one apply plus one plan" satisfied "a chain over at
	// least 2" — a dispatch that promotes exactly one deployment and calls it a
	// pipeline. The name check still covers plan jobs; only the count narrowed.
	planPlusApply := Plan{Stages: []StageRef{
		{Job: "preview", Env: "prod", Action: "plan"},
		{Job: "prod", Env: "prod", Action: "apply", Needs: []string{"preview"}},
	}, Declared: []string{"prod"}}
	err = planPlusApply.NoPipelineErr()
	if err == nil {
		t.Fatal("one apply plus one plan is not a two-stage pipeline")
	}
	if !strings.Contains(err.Error(), "without `action: apply`") {
		t.Errorf("the message must say WHY the count is lower than the job count:\n%s", err)
	}

	// An empty `action:` is not an apply either: llz-terraform.yml gates every apply
	// step on `inputs.action == 'apply'`, so a stage without one runs nothing.
	blank := Plan{Stages: []StageRef{{Job: "dev", Env: "dev"}, {Job: "prod", Env: "prod", Needs: []string{"dev"}}}, Declared: []string{"dev", "prod"}}
	if blank.NoPipelineErr() == nil {
		t.Error("two stages with no `action:` promote nothing, so there is no pipeline")
	}
}

// THE ROUTE BACK TO GREEN, end to end, on the one instance shape this PR is
// about: one deployment, no ranks, three stale stages. `llz env pipeline` must be
// able to reach a state `--check` accepts, using only the deployments that exist.
// It could not before — the fix it printed needed two ranked deployments, which is
// unreachable when you have one.
func TestStaleStagesWithTooFewRanksRegenerateToTheEmptyPipeline(t *testing.T) {
	root := t.TempDir()
	chdir(t, root)
	writeCluster(t, "tf", map[string]string{"prod.tfvars": "region = \"us-ord\"\n"})
	if err := os.MkdirAll(filepath.Join(".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(".github", "workflows", "promote.yml"), threeStageWorkflow)

	plan, err := PlanWorkflow(testDeps(), "tf", "")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.Changed || plan.Content == "" {
		t.Fatal("want a placeholder to write")
	}
	applyPlan(t, plan)

	// After the write the file is stage-less, so the PR gate passes...
	after, err := PlanWorkflow(testDeps(), "tf", "")
	if err != nil {
		t.Fatalf("plan after write: %v", err)
	}
	if err := after.RunnableErr(false); err != nil {
		t.Errorf("the regenerated file must satisfy the PR gate: %v", err)
	}
	if after.Changed {
		t.Error("regenerating twice must be a no-op")
	}
	// ...and dispatching it still fails, because there genuinely is no pipeline.
	if after.RunnableErr(true) == nil {
		t.Error("an empty pipeline must still fail --require-pipeline")
	}
}

// A VALID UNRANKED PIPELINE MUST SURVIVE. An earlier cut rendered the placeholder
// for every tree with <2 ranks, which deleted a working promote.yml: an instance
// declaring dev and prod with two valid stages but no promotionRank — the state
// the old shipped example put people in — had `--check` demand a regeneration that
// then removed both stages, and `llz env add` did the same overwrite with no
// prompt. Unranked is not broken; only a stage naming a deployment that does not
// exist is, and that is the only thing worth overwriting a file to fix.
func TestValidUnrankedStagesAreNotOverwritten(t *testing.T) {
	root := t.TempDir()
	chdir(t, root)
	// Every stage names a declared deployment; NONE of them carries a rank.
	writeCluster(t, "tf", map[string]string{
		"dev.tfvars":     "region = \"us-ord\"\n",
		"staging.tfvars": "region = \"us-ord\"\n",
		"prod.tfvars":    "region = \"us-ord\"\n",
	})
	if err := os.MkdirAll(filepath.Join(".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(".github", "workflows", "promote.yml")
	mustWrite(t, path, threeStageWorkflow)

	plan, err := PlanWorkflow(testDeps(), "tf", "")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Changed || plan.Content != "" {
		t.Fatalf("a valid unranked pipeline must be left alone, got Changed=%v content=%d bytes",
			plan.Changed, len(plan.Content))
	}
	if err := plan.RunnableErr(false); err != nil {
		t.Errorf("...and it must pass the PR gate: %v", err)
	}
	if err := plan.RunnableErr(true); err != nil {
		t.Errorf("...and dispatch, since three real stages ARE a pipeline: %v", err)
	}
	// The write path must not touch it either — `llz env add` calls this with
	// check=false and no prompt.
	applyPlan(t, plan)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != threeStageWorkflow {
		t.Error("the file was rewritten")
	}
}

// The note must describe what the run ACTUALLY did. It used to be shared between
// the leave-alone and the overwrite paths, so a run that wrote nothing announced
// "generating the no-stage placeholder".
func TestNoteDoesNotClaimAWriteThatDidNotHappen(t *testing.T) {
	for _, tc := range []struct {
		name           string
		ranked, onDisk int
		mustNotSay     string
		mustSay        string
	}{
		{"nothing on disk", 1, 0, "generating", "nothing to generate yet"},
		{"valid unranked stages", 0, 3, "generating", "not managing this file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note := unmanagedNote(tc.ranked, tc.onDisk)
			if strings.Contains(note, tc.mustNotSay) {
				t.Errorf("note claims a write that does not happen:\n%s", note)
			}
			if !strings.Contains(note, tc.mustSay) {
				t.Errorf("note must say %q:\n%s", tc.mustSay, note)
			}
		})
	}
}

// The preflight job id must be RESERVED, not merely improbable. A deployment
// called llz-preflight emits a duplicate `jobs:` key: GitHub rejects the whole
// workflow naming neither cause, and workflowStages then hard-errors on it. The
// generator's comment used to assert `llz env add` would never produce the name;
// EnvNameRe accepts it, and nothing checked. This spans the two packages that
// each hold half the claim.
func TestPreflightJobIDIsAReservedDeploymentName(t *testing.T) {
	if err := validate.EnvName(preflightJob); err == nil {
		t.Errorf("%q must be rejected as a deployment name — it is a job id in the generated promote.yml", preflightJob)
	}
	// Fail closed: if EnvNameRe ever stopped accepting the shape, the assertion
	// above would pass for the wrong reason.
	if !validate.EnvNameRe.MatchString(preflightJob) {
		t.Fatalf("%q no longer matches EnvNameRe, so the test above proves nothing", preflightJob)
	}
	// A normal name still works.
	if err := validate.EnvName("staging"); err != nil {
		t.Errorf("the reservation must not reject ordinary names: %v", err)
	}
}

// A stage with no `region:` is odd, not unrunnable — the input is required:false
// on the reusable body. It gets REPORTED, but it must not trigger the destructive
// overwrite: wiping an operator's file over odd-but-legal is the same over-reach
// that once deleted valid unranked pipelines.
func TestNoRegionStageIsReportedButDoesNotTriggerAnOverwrite(t *testing.T) {
	root := t.TempDir()
	chdir(t, root)
	writeCluster(t, "tf", map[string]string{"prod.tfvars": "region = \"us-ord\"\n"})
	if err := os.MkdirAll(filepath.Join(".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "jobs:\n  prod:\n    uses: ./.github/workflows/llz-terraform.yml\n" +
		"    with:\n      region: prod\n" +
		"  odd:\n    uses: ./.github/workflows/llz-terraform.yml\n    with:\n      action: apply\n"
	mustWrite(t, filepath.Join(".github", "workflows", "promote.yml"), body)

	plan, err := PlanWorkflow(testDeps(), "tf", "")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Undeclared) != 1 || plan.Undeclared[0].Job != "odd" {
		t.Errorf("the region-less stage must still be REPORTED: %+v", plan.Undeclared)
	}
	if plan.Changed {
		t.Error("...but must NOT trigger a destructive regeneration")
	}
}

// The three gate arms must fire in most-specific-first order. With no-pipeline
// ahead of drift, a placeholder on disk plus two ranked deployments reported "no
// promotion pipeline to run" and printed a remedy the operator had already done,
// while suppressing the accurate one — regenerate.
func TestGateArmsFireMostSpecificFirst(t *testing.T) {
	drifted := Plan{Changed: true, Declared: []string{"dev", "prod"}} // placeholder on disk, 2 ranks
	err := drifted.RunnableErr(true)
	if err == nil || !strings.Contains(err.Error(), "out of date") {
		t.Errorf("regenerable drift must report drift, not 'no pipeline': %v", err)
	}
	// Undeclared still outranks drift: it names WHICH deployment is wrong.
	both := Plan{Changed: true, Undeclared: []StageRef{{Job: "dev", Env: "dev"}}, Declared: []string{"prod"}}
	if err := both.RunnableErr(true); err == nil || !strings.Contains(err.Error(), "does not declare") {
		t.Errorf("undeclared must outrank drift: %v", err)
	}
}

// Applied() reports on the bytes WRITTEN, not the ones replaced. The first cut
// cleared Undeclared but left the pre-write Stages, so regenerating a two-stage
// pipeline and asking --require-pipeline in the same run exited 1 claiming the
// file it had just written declared no stages.
func TestAppliedReReadsTheWrittenStages(t *testing.T) {
	pre := Plan{
		Content:    renderPromoteWorkflow(testCaller(), []promoStage{{name: "dev", rank: 1}, {name: "prod", rank: 2}}),
		Changed:    true,
		Stages:     nil, // what was on disk before: nothing
		Undeclared: []StageRef{{Job: "gone", Env: "gone"}},
		Declared:   []string{"dev", "prod"},
	}
	got := pre.Applied()
	if len(got.Stages) != 2 {
		t.Errorf("Applied must re-read the written stages, got %+v", got.Stages)
	}
	if len(got.Undeclared) != 0 {
		t.Errorf("the write fixes Undeclared: %+v", got.Undeclared)
	}
	if err := got.RunnableErr(true); err != nil {
		t.Errorf("a freshly written 2-stage pipeline must satisfy --require-pipeline: %v", err)
	}
}

// An UNREADABLE promote.yml must be an error. An ABSENT one must not — the two
// are different answers, and collapsing them reintroduces this gate's own bug one
// layer down: a chmod 000 would turn a failing promote.yml green.
func TestUnreadablePromoteYamlFailsClosedButAbsentDoesNot(t *testing.T) {
	root := t.TempDir()
	chdir(t, root)
	writeCluster(t, "tf", map[string]string{"prod.tfvars": "region = \"us-ord\"\n"})

	// Absent: a real state, no claim made.
	if _, err := PlanWorkflow(testDeps(), "tf", ""); err != nil {
		t.Errorf("an absent promote.yml is not an error: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(".github", "workflows", "promote.yml")
	mustWrite(t, path, threeStageWorkflow)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Skipf("cannot make the file unreadable here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	if os.Geteuid() == 0 {
		t.Skip("running as root — chmod 000 is still readable")
	}
	if _, err := PlanWorkflow(testDeps(), "tf", ""); err == nil {
		t.Error("an unreadable promote.yml must be an error, not a pass")
	}
}

// RunnableErr is what `llz env pipeline` actually calls, so the composition —
// not just the two halves — is what has to be right. The table is the full
// cross-product of the two inputs that decide it.
func TestRunnableErrComposesTheTwoContexts(t *testing.T) {
	twoGood := []StageRef{
		{Job: "dev", Env: "dev", Action: "apply", Needs: []string{"llz-preflight"}},
		{Job: "prod", Env: "prod", Action: "apply", Needs: []string{"dev"}},
	}
	declared := []string{"dev", "prod"}

	for _, tc := range []struct {
		name            string
		plan            Plan
		wantPR, wantDis bool // should it fail as a PR gate / as a dispatch preflight
	}{
		{"a healthy pipeline", Plan{Stages: twoGood, Declared: declared}, false, false},
		// The template's shipped state: nothing claimed, so the PR gate has nothing
		// to falsify — but there is also nothing to dispatch.
		{"no stages at all", Plan{Declared: declared}, false, true},
		// The gsap-apl state. Fails BOTH: --require-pipeline must not mask the more
		// specific undeclared-stage message by short-circuiting on the count first.
		{"stages the spec does not declare", Plan{
			Stages:     twoGood,
			Undeclared: []StageRef{{Job: "dev", Env: "dev"}},
			Declared:   []string{"prod"},
		}, true, true},
		// Two applies and no `needs:` anywhere between them. Fatal in BOTH contexts,
		// unlike the no-pipeline arm: "not configured yet" is a state every instance
		// starts in, "configured with no order" takes an edit to produce — and what it
		// produces is prod applying alongside dev.
		{"stages with no order over them", Plan{
			Stages: []StageRef{
				{Job: "dev", Env: "dev", Action: "apply"},
				{Job: "prod", Env: "prod", Action: "apply"},
			},
			Declared: declared,
		}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.plan.RunnableErr(false) != nil; got != tc.wantPR {
				t.Errorf("RunnableErr(false) failed = %v, want %v", got, tc.wantPR)
			}
			if got := tc.plan.RunnableErr(true) != nil; got != tc.wantDis {
				t.Errorf("RunnableErr(true) failed = %v, want %v", got, tc.wantDis)
			}
		})
	}

	// When both are wrong, the undeclared-stage message is the one that survives:
	// it names the specific deployment, and "there is no pipeline" would send the
	// reader to build one that still would not run.
	both := Plan{Undeclared: []StageRef{{Job: "dev", Env: "dev"}}, Declared: []string{"prod"}}
	if err := both.RunnableErr(true); err == nil || !strings.Contains(err.Error(), "does not declare") {
		t.Errorf("the more specific message must win: %v", err)
	}
}

// The messages an operator reads on an instance that has not been set up at all.
// Both errors have a branch for "zero declared deployments", and both branches
// exist because "declared deployments: " with nothing after it reads as a bug in
// the tool rather than a fact about the repo.
func TestMessagesOnAnInstanceWithNothingDeclared(t *testing.T) {
	undeclared := Plan{Undeclared: []StageRef{{Job: "dev", Env: "dev"}, {Job: "broken", Env: ""}}}
	err := undeclared.UndeclaredErr()
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "no environments/<env>.yaml yet") {
		t.Errorf("must say the instance declares nothing:\n%s", err)
	}
	// A stage with no region: at all gets its own sentence — "applies nothing" and
	// "applies the wrong thing" have different fixes.
	if !strings.Contains(err.Error(), `stage "broken" declares no region: input`) {
		t.Errorf("a region-less stage needs its own message:\n%s", err)
	}

	if err := (Plan{}).NoPipelineErr(); err == nil || !strings.Contains(err.Error(), "llz env add") {
		t.Errorf("NoPipelineErr on a bare instance must point at `llz env add`: %v", err)
	}
}

// A spec that is present but unreadable must ERROR, not fall through to the
// tfvars path and answer from a second source. Silently switching sources would
// let the gate check the workflow against deployments the spec no longer lists.
func TestDeploymentNamesPropagatesASpecError(t *testing.T) {
	d := testDeps()
	d.LoadSpec = func() (*clusterspec.LandingZone, bool, error) {
		return nil, true, fmt.Errorf("landingzone.yaml: bad yaml")
	}
	if _, err := DeploymentNames(d, "tf"); err == nil {
		t.Error("an unreadable spec must be an error, not a fallback to tfvars")
	}
}

// Both preflights must ask for --require-pipeline, and the PR gate must NOT. The
// three call sites are in three different files (generated Go, the shipped
// template, and the vendored reusable body), which is exactly the arrangement
// where one gets updated and the others do not.
func TestPreflightsRequireAPipelineAndThePRGateDoesNot(t *testing.T) {
	const flag = "llz env pipeline --check --require-pipeline"

	generated := renderPromoteWorkflow(testCaller(), []promoStage{{name: "dev", rank: 1}, {name: "prod", rank: 2}})
	if !strings.Contains(generated, flag) {
		t.Errorf("the generated preflight must run %q:\n%s", flag, generated)
	}

	wf := filepath.Join(scaffoldDir(t), ".github", "workflows")
	shipped, err := os.ReadFile(filepath.Join(wf, "promote.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(shipped), flag) {
		t.Errorf("the shipped promote.yml preflight must run %q", flag)
	}

	// The PR gate runs on every pull request of every instance, including the ones
	// that have deliberately not built a pipeline. If it ever grows this flag it
	// fails all of them for being in a supported state.
	body, err := os.ReadFile(filepath.Join(wf, "llz-terraform.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "--require-pipeline") {
		t.Error("promote-pipeline-drift must NOT pass --require-pipeline — it would fail every " +
			"instance that has not configured a promotion pipeline, which is a supported state")
	}
}

// THE SKEW-ORDER RULE, pinned in all three preflights. llz-terraform.yml records
// it in prose — "Any new verb added here belongs BELOW it" — after a job died on
// `unknown command` and its actionable skew message never ran. `--require-pipeline`
// is a new flag, so the preflight walked straight into the same trap: an image
// that predates it answers `unknown flag: --require-pipeline`, and stage 1 never
// starts. assert-image-fresh has shipped for many releases, so it resolves in the
// old image and says the useful thing — but only if it runs first.
func TestPreflightChecksImageSkewBeforeTheNewFlag(t *testing.T) {
	shipped, err := os.ReadFile(filepath.Join(scaffoldDir(t), ".github", "workflows", "promote.yml"))
	if err != nil {
		t.Fatal(err)
	}
	// The DOC is in the corpus too. It carries a fourth copy of this job, and the
	// previous cut of this test checked the three real ones only — so the doc drifted
	// out of step with the rule in the same commit that established it.
	doc, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "environments-and-promotion.md"))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"generated":   renderPromoteWorkflow(testCaller(), []promoStage{{name: "dev", rank: 1}, {name: "prod", rank: 2}}),
		"placeholder": renderPlaceholderWorkflow(),
		"shipped":     string(shipped),
		"doc":         string(doc),
	} {
		// The RUN STEPS, not any prose above them — the comment explaining this rule
		// names the flag too, and matching that would let the steps sit in either
		// order while the test stayed green.
		skew := strings.Index(body, "llz ci assert-image-fresh")
		newFlag := strings.Index(body, "llz env pipeline --check --require-pipeline")
		if skew < 0 {
			t.Errorf("%s: preflight must check image skew at all:\n%s", name, body)
			continue
		}
		if newFlag < 0 || skew > newFlag {
			t.Errorf("%s: assert-image-fresh must run BEFORE --require-pipeline (skew@%d, flag@%d)", name, skew, newFlag)
		}
	}
}

// The generated preflight restates the actions/checkout pin that the delivered
// workflows carry in YAML, and nothing else compares the two. A bump that edits
// the workflows leaves every regenerated instance pipeline on the old SHA — so
// the two copies are compared here, against the real delivered files.
func TestCheckoutActionMatchesTheDeliveredWorkflows(t *testing.T) {
	sha := strings.TrimSpace(strings.SplitN(checkoutAction, "#", 2)[0])
	dir := filepath.Join(scaffoldDir(t), ".github", "workflows")
	entries, err := filepath.Glob(filepath.Join(dir, "*.yml"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no delivered workflows found: %v", err)
	}
	seen := 0
	for _, f := range entries {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, "uses:") || !strings.Contains(line, "actions/checkout@") {
				continue
			}
			seen++
			// The value after `uses:`, with any trailing `# vX` comment dropped —
			// the step may be a list item (`- uses:`) or a bare key.
			got := strings.TrimSpace(strings.SplitN(line, "uses:", 2)[1])
			got = strings.TrimSpace(strings.SplitN(got, "#", 2)[0])
			if got != sha {
				t.Errorf("%s pins %q; promote.go's checkoutAction is %q — bump both",
					filepath.Base(f), got, sha)
			}
		}
	}
	// Fail closed: a glob that matched no checkout step would pass this silently.
	if seen == 0 {
		t.Fatal("scanned 0 actions/checkout steps — the corpus is empty, so this proves nothing")
	}
}

// repoRoot walks up for the tree containing instance-template/, so the docs can
// be read without a relative literal that breaks when this package moves.
func repoRoot(t *testing.T) string {
	t.Helper()
	return filepath.Dir(scaffoldDir(t))
}

// scaffoldDir walks up for instance-template/ rather than hardcoding a relative
// literal, which would point at nothing the first time this package moves — the
// same helper, and the same reason, as registry/ownpaths_test.go.
func scaffoldDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		cand := filepath.Join(dir, "instance-template")
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find instance-template/ from the test's working directory")
	return ""
}

// AN EXPRESSION `region:` MUST NOT BE TREATED AS A MISSING DEPLOYMENT. This is the
// third time this change generalised a remedy into a rule and destroyed something,
// and the second time it was this exact write: round 2 stopped the placeholder
// overwriting valid UNRANKED stages by gating on `len(undeclared) > 0`, which still
// read `region: ${{ inputs.target }}` as "names a deployment that does not exist"
// — because `${{ … }}` is a non-empty string that is not in the spec.
//
// Reproduced end to end with a branch-built llz before this fix: two stages in,
// `--check` fails, run the remedy it prints, zero stages out.
func TestExpressionRegionIsNotOverwritten(t *testing.T) {
	root := t.TempDir()
	chdir(t, root)
	writeCluster(t, "tf", map[string]string{
		"dev.tfvars":  "region = \"us-ord\"\n",
		"prod.tfvars": "region = \"us-ord\"\n",
	})
	if err := os.MkdirAll(filepath.Join(".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `name: Promote (operator-maintained)
on:
  workflow_dispatch:
    inputs:
      target:
        type: string
jobs:
  first:
    uses: ./.github/workflows/llz-terraform.yml
    with:
      action: apply
      region: dev
  second:
    needs: first
    uses: ./.github/workflows/llz-terraform.yml
    with:
      action: apply
      region: ${{ inputs.target }}
`
	path := filepath.Join(".github", "workflows", "promote.yml")
	mustWrite(t, path, body)

	plan, err := PlanWorkflow(testDeps(), "tf", "")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Content != "" || plan.Changed {
		t.Fatalf("an expression region: must never authorise a write; got Changed=%v content:\n%s", plan.Changed, plan.Content)
	}
	// NOT an error either: there is no name to compare, so failing would assert a
	// falsehood llz has no evidence for — and no edit makes an expression resolvable
	// at check time, so the failure would have no reachable remedy.
	if err := plan.RunnableErr(false); err != nil {
		t.Errorf("an unverifiable stage is not a failing stage: %v", err)
	}
	if len(plan.Unresolved) != 1 || plan.Unresolved[0].Job != "second" {
		t.Fatalf("the expression stage must be reported as unresolved; got %+v", plan.Unresolved)
	}
	if len(plan.Undeclared) != 0 {
		t.Errorf("an expression stage must NOT be counted undeclared; got %+v", plan.Undeclared)
	}
	// And the green exit must not claim it was checked.
	advisories := strings.Join(plan.Advisories(), "\n")
	if !strings.Contains(advisories, "UNVERIFIED") || !strings.Contains(advisories, "second") {
		t.Errorf("a passing check must disclose what it could not verify; got:\n%s", advisories)
	}
}

// A LITERAL name that is not declared still authorises the write — the gsap-apl
// route back to green. The twin of the test above: both directions of the same
// decision, which is what was missing each of the three times this broke.
func TestLiteralUndeclaredNameStillAuthorisesTheWrite(t *testing.T) {
	root := t.TempDir()
	chdir(t, root)
	writeCluster(t, "tf", map[string]string{"prod.tfvars": "region = \"us-ord\"\n"})
	if err := os.MkdirAll(filepath.Join(".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(".github", "workflows", "promote.yml"), threeStageWorkflow)
	plan, err := PlanWorkflow(testDeps(), "tf", "")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.Changed || plan.Content == "" {
		t.Fatal("a stage naming a deployment that does not exist must still be replaceable")
	}
	// The note must count what authorised the write, not everything flagged. It used
	// to print len(undeclared), which also holds the stages llz merely could not
	// resolve — none of which are a reason to overwrite anything.
	if !strings.Contains(plan.Note, "2 stage(s) name deployments") {
		t.Errorf("the note must count the stages that authorised the write; got %q", plan.Note)
	}
}

// `needs:` IS THE PRODUCT. promote.yml's own header says the only things it adds
// over dispatching terraform.yml per deployment are the ordering and the per-stage
// green gate — and until this arm existed the gate measured neither: a two-stage
// file with no `needs:` between them reported "in sync … and every stage names a
// declared deployment" and exited 0, while applying dev and prod simultaneously.
// Same shape as the failure this whole change is about, with the names all correct.
func TestUnorderedStagesFailInBothContexts(t *testing.T) {
	unchained, err := workflowStages([]byte(`jobs:
  dev:
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: apply, region: dev}
  prod:
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: apply, region: prod}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := Plan{Stages: unchained, Declared: []string{"dev", "prod"}}
	for _, requirePipeline := range []bool{false, true} {
		err := p.RunnableErr(requirePipeline)
		if err == nil {
			t.Fatalf("RunnableErr(%v): an unordered pipeline must fail in both contexts", requirePipeline)
		}
		if !strings.Contains(err.Error(), "no order over them") {
			t.Errorf("RunnableErr(%v) = %v, want the ordering message", requirePipeline, err)
		}
	}

	// A PARTIAL chain stays legal. One stage fanning out to two is an operator's
	// choice, and a hand-maintained file is not required to look like the generated
	// one — only zero edges cannot be a choice, because nothing is left for the file
	// to mean. Narrowing this is the same restraint the unranked case needed.
	fanOut, err := workflowStages([]byte(`jobs:
  dev:
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: apply, region: dev}
  eu:
    needs: dev
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: apply, region: eu}
  us:
    needs: dev
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: apply, region: us}
`))
	if err != nil {
		t.Fatal(err)
	}
	fan := Plan{Stages: fanOut, Declared: []string{"dev", "eu", "us"}}
	if err := fan.RunnableErr(true); err != nil {
		t.Errorf("a fan-out is an ordering, not the absence of one: %v", err)
	}
}

// `needs:` is a scalar for one dependency and a sequence for several. The generated
// file uses the scalar form, so a decoder that read only sequences would report
// every generated pipeline as unordered — the new arm failing the very files it was
// added to bless.
func TestNeedsDecodesBothYAMLForms(t *testing.T) {
	st, err := workflowStages([]byte(`jobs:
  a:
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: apply, region: a}
  b:
    needs: a
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: apply, region: b}
  c:
    needs: [a, b]
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: apply, region: c}
`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, s := range st {
		got[s.Job] = s.Needs
	}
	if len(got["b"]) != 1 || got["b"][0] != "a" {
		t.Errorf("scalar needs: = %v, want [a]", got["b"])
	}
	if len(got["c"]) != 2 {
		t.Errorf("sequence needs: = %v, want [a b]", got["c"])
	}

	// A valueless `needs:` must not read as a dependency on a job called "" — that
	// would be enough to make missingPreflight believe the stage chains from something
	// and suppress the advisory. yaml.v3 gives this for free (it does not call a
	// custom unmarshaler for a null node), which is why there is no code here for it:
	// MEASURED, and the assertion stays as the pin, because "the library happens to do
	// the right thing" is exactly the kind of dependency worth noticing if it changes.
	empty, err := workflowStages([]byte("jobs:\n  a:\n    needs:\n    uses: ./.github/workflows/llz-terraform.yml\n    with: {action: apply, region: a}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 1 || len(empty[0].Needs) != 0 {
		t.Errorf("a valueless needs: must yield no dependencies; got %+v", empty)
	}
	if !missingPreflight(empty) {
		t.Error("a stage depending on nothing has no preflight in front of it")
	}
}

// The generated pipeline must satisfy the ordering arm and carry a preflight, or
// `llz env pipeline` would write files its own `--check` rejects and advise about.
func TestGeneratedWorkflowIsOrderedAndPreflighted(t *testing.T) {
	out := renderPromoteWorkflow(promoCaller{uses: localTerraformUses, instanceRepo: "myorg/inst"},
		[]promoStage{{name: "dev", rank: 1}, {name: "staging", rank: 2}, {name: "prod", rank: 3}})
	st, err := workflowStages([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	p := Plan{Stages: st, Declared: []string{"dev", "staging", "prod"}}
	if err := p.RunnableErr(true); err != nil {
		t.Fatalf("the generator must satisfy every arm of its own check: %v", err)
	}
	if adv := p.Advisories(); len(adv) != 0 {
		t.Errorf("the generated file must not need advising about: %v", adv)
	}
}

// The missing-preflight advisory is REPORTED, NEVER FATAL. Every promote.yml
// generated before the preflight existed lacks it, `llz env pipeline` leaves a valid
// unranked pipeline alone by design, and promote.yml is `owned` so an upgrade will
// not add the job either — so failing on it would be unearned three times over.
// This is the only channel that reaches those instances at all.
func TestMissingPreflightIsAdvisedNotFailed(t *testing.T) {
	st, err := workflowStages([]byte(threeStageWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	p := Plan{Stages: st, Declared: []string{"dev", "staging", "prod"}}
	if err := p.RunnableErr(true); err != nil {
		t.Fatalf("a chained pipeline without a preflight still runs: %v", err)
	}
	adv := strings.Join(p.Advisories(), "\n")
	if !strings.Contains(adv, "no preflight") {
		t.Errorf("the operator must be told the dispatch is ungated; got:\n%s", adv)
	}
	if !strings.Contains(adv, "`owned`") {
		t.Errorf("and told why an upgrade will not fix it for them; got:\n%s", adv)
	}
}

// THE DELIVERED FILE'S PROSE MUST NAME ITS ACTUAL CLASS. The header tells an
// adopter what `llz upgrade` will do to their promote.yml, and for one commit it
// told them the opposite: the class had been changed to `owned` + `_skip_if_exists`
// in the same commit that left the header explaining how to resolve the `merge`
// conflict markers that can no longer occur. Both halves of that claim are
// machine-readable, so nothing needs to rely on someone re-reading the comment.
func TestDeliveredHeaderNamesItsRealTemplateClass(t *testing.T) {
	// manifest.Load takes the SCAFFOLD root — .template-manifest lives inside
	// instance-template/, alongside the files it classifies.
	m, err := manifest.Load(scaffoldDir(t))
	if err != nil {
		t.Fatalf("load .template-manifest: %v", err)
	}
	rel := ".github/workflows/promote.yml"
	class := m.Classify(rel)
	if class == "" {
		t.Fatalf("%s is unclassified in .template-manifest", rel)
	}
	body, err := os.ReadFile(filepath.Join(scaffoldDir(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	head := string(body)
	for _, other := range []string{"managed", "merge", "owned"} {
		claim := "`" + other + "`-class"
		if strings.Contains(head, claim) && other != class {
			t.Errorf("%s calls itself a %s file, but .template-manifest classifies it %q — "+
				"the header is what an operator reads to decide whether an upgrade will rewrite their pipeline",
				rel, claim, class)
		}
	}
	// And the true class must actually be stated: an `owned` file whose header says
	// nothing leaves the adopter to guess, which is what the wrong claim replaced.
	if !strings.Contains(head, "`"+class+"`") {
		t.Errorf("%s never mentions its class (%q); the header must say whether `llz upgrade` touches it", rel, class)
	}
}

// A TEMPLATE-REPO CHECKOUT ANSWERS "NOTHING TO CHECK" TO EVERY FLAG COMBINATION.
// PlanWorkflow reads nothing there, so any verdict is about a comparison that never
// ran. The Path guard used to sit in front of the success LINE only, so
// `--check --require-pipeline` at the repo root failed with "this instance declares
// no deployments yet" — about a tree that is not an instance and declares nothing by
// definition. That is the gate's own original sin, one flag over.
func TestTemplateCheckoutIsNeverJudged(t *testing.T) {
	for _, requirePipeline := range []bool{false, true} {
		lines, err := (Plan{}).CheckReport(true, requirePipeline)
		if err != nil {
			t.Errorf("CheckReport(check, %v) on a template checkout = %v, want no verdict", requirePipeline, err)
		}
		if len(lines) != 1 || !strings.Contains(lines[0], "not an instance checkout") {
			t.Errorf("CheckReport(check, %v) = %q", requirePipeline, lines)
		}
	}
	// And it says nothing at all when it was not asked a question.
	if lines, err := (Plan{}).CheckReport(false, false); err != nil || len(lines) != 0 {
		t.Errorf("a bare `llz env pipeline` on a template checkout must be silent; got %q, %v", lines, err)
	}
}

// ADVISORIES COME BEFORE THE SUCCESS LINE, AND NOTHING COMES BEFORE AN ERROR.
// Ordering the other way would print "every stage names a declared deployment"
// above the disclosure that one of them could not be checked, which is the same
// abstention-as-agreement in typographical form.
func TestCheckReportOrdersItsOutput(t *testing.T) {
	st, err := workflowStages([]byte(`jobs:
  llz-preflight:
    runs-on: ubuntu-latest
    steps: [{run: llz env pipeline --check}]
  dev:
    needs: llz-preflight
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: apply, region: dev}
  prod:
    needs: dev
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: apply, region: "${{ vars.PROD }}"}
`))
	if err != nil {
		t.Fatal(err)
	}
	_, unresolved := undeclaredStages(st, []string{"dev"})
	p := Plan{Path: ".github/workflows/promote.yml", Stages: st, Unresolved: unresolved, Declared: []string{"dev"}}

	lines, err := p.CheckReport(true, true)
	if err != nil {
		t.Fatalf("an unverifiable stage is not a failure: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("want the advisory then the success line; got %q", lines)
	}
	if !strings.Contains(lines[0], "UNVERIFIED") {
		t.Errorf("the advisory must come first; got %q", lines[0])
	}
	// The claim must be qualified rather than absolute — it did not check that stage.
	if !strings.Contains(lines[1], "unverified — see above") {
		t.Errorf("the success line must not claim coverage it does not have; got %q", lines[1])
	}

	// A failing plan says nothing on the way out: printing a partial verdict above an
	// error invites reading the first line and stopping.
	bad := Plan{Path: p.Path, Stages: st, Undeclared: []StageRef{{Job: "dev", Env: "dev"}}, Declared: []string{"prod"}}
	lines, err = bad.CheckReport(true, true)
	if err == nil {
		t.Fatal("an undeclared stage must fail")
	}
	if len(lines) != 0 {
		t.Errorf("nothing is printed alongside a failure; got %q", lines)
	}
}

// THE GENERATOR MUST NOT CORRUPT ITS OWN OUTPUT. Found by running the remedy this
// change advertises twice on the same tree: `llz env pipeline` wrote a stub with an
// empty `instance_repo: `, and the next run's regex — `\s*`, which matches newlines
// — skipped that empty value and captured the following key, rendering
// `instance_repo: action:`. That is not valid YAML, so from then on every `--check`
// died with "parse promote.yml: mapping values are not allowed in this context",
// about a file llz itself had written, with no remedy printed.
//
// Two fixes. This test covers the second — a stub that names no instance_repo is no
// longer accepted as the answer, so it falls through to .copier-answers.yml, which is
// what the chain is for. TestInstanceRepoCaptureStopsAtTheLineEnd covers the first,
// which this one cannot reach: with the empty value never written, the greedy regex
// has nothing to over-read. Both directions, separately pinned — the omission this
// branch has now made four times is a fix whose gate only proves the other fix.
func TestRegeneratingTwiceIsStableAndNeverCorrupts(t *testing.T) {
	root := t.TempDir()
	chdir(t, root)
	writeCluster(t, "tf", map[string]string{
		"dev.tfvars":  "region = \"us-ord\"\npromotion_rank = 1\n",
		"prod.tfvars": "region = \"us-ord\"\npromotion_rank = 2\n",
	})
	if err := os.MkdirAll(filepath.Join(".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A hand-maintained promote.yml: a real `uses:`, and no instance_repo anywhere.
	mustWrite(t, filepath.Join(".github", "workflows", "promote.yml"), `name: Promote
on: {workflow_dispatch: {}}
jobs:
  dev:
    uses: ./.github/workflows/llz-terraform.yml
    with:
      action: apply
      region: dev
`)

	// .copier-answers.yml is the third and last source in resolveCaller's chain, and
	// with the stub no longer accepted as an answer it is the one that supplies this.
	d := testDeps()
	d.InstanceRepo = func() string { return "myorg/my-instance" }

	var last string
	for i := 0; i < 3; i++ {
		plan, err := PlanWorkflow(d, "tf", "")
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if i > 0 && plan.Changed {
			t.Fatalf("run %d: regenerating an already-generated file must be a no-op, got a rewrite:\n%s", i, plan.Content)
		}
		if plan.Changed {
			applyPlan(t, plan)
			last = plan.Content
		}
		// Whatever is on disk must always parse — a generator that emits YAML its own
		// reader rejects has no way back.
		body, err := os.ReadFile(filepath.Join(".github", "workflows", "promote.yml"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := workflowStages(body); err != nil {
			t.Fatalf("run %d left an unparseable promote.yml: %v\n%s", i, err, body)
		}
	}
	// And the repo came from .copier-answers.yml, not from an empty capture.
	if strings.Contains(last, "instance_repo: action:") || strings.Contains(last, "instance_repo: \n") {
		t.Errorf("instance_repo was not resolved:\n%s", last)
	}
}

// The `instance_repo:` capture must stop at the end of its line. `\s` matches
// newlines, so against an empty value the original skipped the break and captured
// the NEXT key — turning a stub llz had written into `instance_repo: action:`, which
// no YAML parser accepts and no message explained.
//
// Tested at the regex rather than through the generator on purpose: the sibling fix
// stops an empty value ever being written, so end-to-end this line is unreachable —
// and an unreachable defect left in place is one refactor away from reachable again.
func TestInstanceRepoCaptureStopsAtTheLineEnd(t *testing.T) {
	empty := "    with:\n      instance_repo: \n      action: apply\n      region: dev\n"
	if m := reInstanceErr.FindStringSubmatch(empty); m != nil {
		t.Errorf("an empty instance_repo: must capture nothing, not %q from the next line", m[1])
	}
	filled := "    with:\n      instance_repo: myorg/inst\n      action: apply\n"
	m := reInstanceErr.FindStringSubmatch(filled)
	if m == nil || m[1] != "myorg/inst" {
		t.Errorf("a real instance_repo: must still be captured; got %q", m)
	}
}

// ONE EDGE TO A PLAN JOB IS NOT AN ORDERING. Found reviewing the ordering arm added
// above: it built the job-id set from every stage, so two applies that both
// `needs:` a shared `plan` preview job each "had" a dependency and the check passed
// — while dev and prod still started together, which is the failure the arm exists
// to catch. The edge has to be between two APPLIES.
func TestOrderingEdgeMustBeBetweenApplies(t *testing.T) {
	st, err := workflowStages([]byte(`jobs:
  preview:
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: plan, region: prod}
  dev:
    needs: preview
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: apply, region: dev}
  prod:
    needs: preview
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: apply, region: prod}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := Plan{Path: "x", Stages: st, Declared: []string{"dev", "prod"}}
	if err := p.RunnableErr(false); err == nil {
		t.Error("dev and prod both hanging off a plan job still apply simultaneously")
	}

	// And the generated shape's `needs: llz-preflight` is a GATE, not an ordering —
	// so it must not be what satisfies the arm either. Here stage 2 chains to stage 1,
	// which is the real chain, and that is what makes this pass.
	chained, err := workflowStages([]byte(`jobs:
  dev:
    needs: llz-preflight
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: apply, region: dev}
  prod:
    needs: dev
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: apply, region: prod}
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := (Plan{Path: "x", Stages: chained, Declared: []string{"dev", "prod"}}).RunnableErr(true); err != nil {
		t.Errorf("a real chain must pass: %v", err)
	}
}

// THE GREEN SENTENCE MAY ONLY CLAIM WHAT IT COMPARED. The first cut of the ordering
// arm printed "every stage names a declared deployment, and the stages are ordered"
// for a promote.yml with NO STAGES — the state every fresh instance is in, so the
// one the PR gate reports on most often — asserting two comparisons with nothing to
// run against. Same abstention-as-agreement, committed by the line announcing the
// fix.
func TestPassingMessageClaimsOnlyWhatItChecked(t *testing.T) {
	stageless, err := (Plan{Path: "x", Declared: []string{"prod"}}).CheckReport(true, false)
	if err != nil {
		t.Fatal(err)
	}
	msg := stageless[len(stageless)-1]
	if strings.Contains(msg, "stage names a declared deployment") || strings.Contains(msg, "ordered") {
		t.Errorf("a stage-less file has no stages to make claims about; got %q", msg)
	}
	if !strings.Contains(msg, "declares no stages") {
		t.Errorf("it must say what it DID find; got %q", msg)
	}

	// One stage: names are compared, ordering is not — there is nothing to order.
	one := Plan{Path: "x", Stages: []StageRef{{Job: "prod", Env: "prod", Action: "apply"}}, Declared: []string{"prod"}}
	lines, err := one.CheckReport(true, false)
	if err != nil {
		t.Fatal(err)
	}
	msg = lines[len(lines)-1]
	if !strings.Contains(msg, "stage names a declared deployment") {
		t.Errorf("one stage IS a name comparison; got %q", msg)
	}
	if strings.Contains(msg, "ordered") {
		t.Errorf("a single stage has no order to check; got %q", msg)
	}

	// Two chained applies: every clause is earned.
	two := Plan{Path: "x", Stages: []StageRef{
		{Job: "dev", Env: "dev", Action: "apply"},
		{Job: "prod", Env: "prod", Action: "apply", Needs: []string{"dev"}},
	}, Declared: []string{"dev", "prod"}}
	lines, err = two.CheckReport(true, true)
	if err != nil {
		t.Fatal(err)
	}
	if msg = lines[len(lines)-1]; !strings.Contains(msg, "the stages are ordered") {
		t.Errorf("a real chain must be claimed as one; got %q", msg)
	}
}

// A FILE MADE ENTIRELY OF EXPRESSIONS COMPARED NOTHING, and the green sentence said
// "every stage names a declared deployment" about it — on a tree that may declare
// none. Claiming a comparison over an empty set is the same defect as claiming one
// over no stages, one level finer, and the trailing "(N unverified)" caveat does not
// repair a clause that was vacuous to begin with.
func TestGreenSentenceOverAllExpressionStages(t *testing.T) {
	st, err := workflowStages([]byte(`jobs:
  a:
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: apply, region: "${{ inputs.a }}"}
  b:
    needs: a
    uses: ./.github/workflows/llz-terraform.yml
    with: {action: apply, region: "${{ inputs.b }}"}
`))
	if err != nil {
		t.Fatal(err)
	}
	bad, unresolved := undeclaredStages(st, nil) // NO declared deployments at all
	if len(bad) != 0 || len(unresolved) != 2 {
		t.Fatalf("expressions are unresolvable, not undeclared; got bad=%+v unresolved=%+v", bad, unresolved)
	}
	lines, err := Plan{Path: "x", Stages: st, Unresolved: unresolved}.CheckReport(true, true)
	if err != nil {
		t.Fatalf("nothing here is checkably wrong: %v", err)
	}
	msg := lines[len(lines)-1]
	if strings.Contains(msg, "names a declared deployment") {
		t.Errorf("no stage was compared to anything; got %q", msg)
	}
	if !strings.Contains(msg, "every stage is an expression") {
		t.Errorf("it must say what it could not do; got %q", msg)
	}
}

// The leave-alone note said "all naming declared deployments" about a file whose
// expression stages were never compared. That path is reached when nothing
// AUTHORISES a rewrite, which is not the same as everything having been checked —
// and it is the note printed to exactly the operators the leave-alone path exists
// to protect.
func TestUnmanagedNoteDoesNotClaimEveryStageWasChecked(t *testing.T) {
	note := unmanagedNote(0, 2)
	if strings.Contains(note, "all naming declared deployments") {
		t.Errorf("the note must not claim a comparison the expression stages never got; %q", note)
	}
	if !strings.Contains(note, "none naming a deployment this instance does not have") {
		t.Errorf("it must state the weaker fact it actually knows; %q", note)
	}
}

// THE REMEDY MUST NOT BE A TRAP. `llz env add` regenerates promote.yml as its last
// step, and while ANY stage still names a deployment that does not exist the
// regeneration writes the empty placeholder. An operator told to "create the missing
// deployment" who has two of them therefore loses the pipeline on the FIRST add,
// having done exactly what the error said. The message names all of them and says
// what happens in between.
func TestMultipleMissingDeploymentsAreAllNamed(t *testing.T) {
	p := Plan{
		Undeclared: []StageRef{{Job: "dev", Env: "dev"}, {Job: "staging", Env: "staging"}},
		Declared:   []string{"prod"},
	}
	err := p.UndeclaredErr()
	if err == nil {
		t.Fatal("want a failure")
	}
	for _, want := range []string{"llz env add dev", "llz env add staging", "while any one of them"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message must contain %q:\n%s", want, err)
		}
	}

	// One missing deployment has no such ordering hazard, and must not be dressed up
	// with a warning that does not apply to it.
	one := Plan{Undeclared: []StageRef{{Job: "dev", Env: "dev"}}, Declared: []string{"prod"}}
	if msg := one.UndeclaredErr().Error(); strings.Contains(msg, "while any one of them") {
		t.Errorf("a single missing deployment has no in-between state:\n%s", msg)
	}

	// A stage flagged only for having no `region:` is not something `llz env add` can
	// create, so it must not be counted into the "create ALL N" instruction.
	noRegion := Plan{
		Undeclared: []StageRef{{Job: "dev", Env: "dev"}, {Job: "broken", Env: ""}},
		Declared:   []string{"prod"},
	}
	// Asserting on the COUNT alone would not pin this: gating on len(p.Undeclared)
	// instead of the missing subset still prints "create ALL 1", which no check for
	// "create ALL 2" would notice. Only one deployment is actually missing here, so
	// the multi-deployment branch must not be taken at all.
	if msg := noRegion.UndeclaredErr().Error(); strings.Contains(msg, "create ALL") {
		t.Errorf("a region-less stage is not a missing deployment — one missing means the single-deployment remedy:\n%s", msg)
	}
}
