package cli

// delivered_workflow_coverage_test.go — two gates against the same failure
// class, which is the one this whole file tree keeps meeting: A DELIVERED
// SURFACE THAT NOTHING EXERCISES IS INDISTINGUISHABLE FROM ONE THAT WORKS.
//
// The evidence, all of it from real instances rather than reasoning:
//
//   • The instance's tf-lint and checkov jobs ran `make tf-lint` against a
//     scaffold with no Makefile, for several releases. Both are gated on
//     `pull_request`, and the example instance is driven by force-push and
//     dispatch — so the jobs had never once executed.
//   • The instance's credential-rotation workflow produced `startup_failure`
//     nightly for months: the caller job held less permission than the callee
//     asked for, so GitHub killed the run before any job existed. No jobs, no
//     annotations, no notification. Nothing in this repo runs that workflow.
//   • v0.0.42 made TF_STATE_ENCRYPTION_PASSPHRASE required. The adopter learned
//     about it from a failed `terraform init` on the PR after the upgrade.
//
// Every one of those is the same shape: a claim about the delivered tree that
// nothing checked, in a place nothing ran. TestDeliveredWorkflowCommands (the
// sibling file) covers the command words. These two cover the other two halves —
// which entry points are exercised at all, and whether the one job that CANNOT
// read its own requirement list from the binary has drifted from it.

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/configreadiness"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/releasepublish"
)

// ── Gate 1: repo-readiness's env: block vs the requirement table ─────────────

// deliveredPipeline is the workflow carrying the repo-readiness job.
const deliveredPipeline = "../../../instance-template/.github/workflows/llz-terraform.yml"

// TestDeliveredJobCoversRepoLevelRequirements pins the ONE thing
// `llz ci require-repo-config` cannot do for itself.
//
// The verb reads the requirement table, so the SET it checks can never drift.
// But a GitHub Actions job only sees a secret it names — there is no way to splat
// them — so the `env:` block in the delivered YAML is a hand-maintained copy of
// part of that table. Add a repo-level required secret to the table and the verb
// starts demanding it while the workflow never supplies it: the gate then fails
// on every instance, forever, for a value that is actually set. That is worse
// than the gap it was added to close, and it is invisible from the Go side.
func TestDeliveredJobCoversRepoLevelRequirements(t *testing.T) {
	raw, err := os.ReadFile(deliveredPipeline)
	if err != nil {
		t.Fatalf("read %s: %v", deliveredPipeline, err)
	}
	body := string(raw)

	if !strings.Contains(body, "llz ci require-repo-config") {
		t.Fatal("the delivered pipeline no longer runs `llz ci require-repo-config` — " +
			"the upgrade-prerequisite gate is gone and this test would pass having checked nothing")
	}

	names := configreadiness.RepoLevelRequirementNames()
	if len(names) == 0 {
		t.Fatal("the requirement table lists no repo-level required values — " +
			"either the table moved or the filter is wrong; this gate would pass vacuously")
	}

	// SCOPED TO THE JOB'S OWN env:, not to the file. Searching the whole file is
	// what the first draft did, and it passed while TF_STATE_ENDPOINT was
	// unreachable from this job: the workflow-level block exports that variable as
	// AWS_ENDPOINT_URL_S3, so `vars.TF_STATE_ENDPOINT` appeared in the file under a
	// DIFFERENT name and the substring matched anyway. The verb looks values up by
	// name, in the environment of the step it runs in — so that is the text this
	// gate has to read, or it is checking a coincidence.
	env := jobEnvBlock(t, body, "repo-readiness")
	for _, n := range names {
		// A requirement arrives as `NAME: ${{ secrets.NAME }}` or `${{ vars.NAME }}`
		// depending on its kind; both put the value in the environment under NAME,
		// which is all the verb needs.
		mapped := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(n) + `:\s*\$\{\{\s*(secrets|vars)\.` + regexp.QuoteMeta(n) + `\s*\}\}`)
		if !mapped.MatchString(env) {
			t.Errorf("%s is a REQUIRED repo-level value but the repo-readiness job does not map it into env: "+
				"as `%s: ${{ secrets.%s }}` (or vars.%s) — `llz ci require-repo-config` reads it from the "+
				"environment BY NAME, so it would report a correctly configured instance as missing it.\n"+
				"repo-readiness env: block was:\n%s",
				n, n, n, n, env)
		}
	}
}

// jobEnvBlock returns the `env:` mapping of one job in a workflow file. Both
// blocks are located by indentation, which is what YAML nesting is — a job is a
// 4-space key under `jobs:`, its `env:` a 6-space key under that, and each block
// ends at the next key of its own depth or shallower.
func jobEnvBlock(t *testing.T, body, job string) string {
	t.Helper()
	jobRe := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(job) + `:\s*$`)
	loc := jobRe.FindStringIndex(body)
	if loc == nil {
		t.Fatalf("no `%s:` job in %s — the gate it backs is gone, and this test would pass "+
			"having read nothing", job, deliveredPipeline)
	}
	rest := body[loc[1]:]
	if end := regexp.MustCompile(`(?m)^  [a-zA-Z_-]+:`).FindStringIndex(rest); end != nil {
		rest = rest[:end[0]] // stop at the next job
	}
	envLoc := regexp.MustCompile(`(?m)^    env:\s*$`).FindStringIndex(rest)
	if envLoc == nil {
		t.Fatalf("job %q has no `env:` block, so it maps no secret into the environment "+
			"`llz ci require-repo-config` reads", job)
	}
	envBody := rest[envLoc[1]:]
	if end := regexp.MustCompile(`(?m)^    [a-zA-Z_-]+:`).FindStringIndex(envBody); end != nil {
		envBody = envBody[:end[0]] // stop at the job's next key (steps:, if:, …)
	}
	return envBody
}

// ── Gate 2: the draft skip and the trigger that makes it recoverable ─────────

// deliveredCaller is the thin stub that owns the pull_request TRIGGER, in a
// different manifest class from the pipeline body that owns the draft SKIP.
const deliveredCaller = "../../../instance-template/.github/workflows/terraform.yml"

// TestDraftSkipHasItsReadyForReviewTrigger couples two files that a copier
// upgrade moves by DIFFERENT rules.
//
// plan-cluster-pr skips draft PRs because it writes Terraform state with nothing
// serializing it against a concurrent apply. That skip is only recoverable
// because `ready_for_review` is in terraform.yml's trigger types — it is NOT in
// GitHub's default set (opened / synchronize / reopened), so without it taking a
// PR out of draft fires no event at all and the plan an operator just asked for
// never runs. They would have to push an empty commit, with nothing on screen
// explaining why.
//
// THE TWO FILES ARE NOT DELIVERED ALIKE. llz-terraform.yml is `managed` — copier
// overwrites it from a clean render, so the skip always lands. terraform.yml is
// `merge` — it carries jinja and an adopter may have edited it, so the trigger
// arrives through a 3-way merge that can decline. An adopter can therefore end up
// with the skip and not the trigger. This test cannot reach that adopter; what it
// can do is guarantee the template never SHIPS the halves out of step, so the
// only way to reach the broken combination is a local edit — which `llz upgrade`
// reports as a conflict.
func TestDraftSkipHasItsReadyForReviewTrigger(t *testing.T) {
	body, err := os.ReadFile(deliveredPipeline)
	if err != nil {
		t.Fatalf("read %s: %v", deliveredPipeline, err)
	}
	caller, err := os.ReadFile(deliveredCaller)
	if err != nil {
		t.Fatalf("read %s: %v", deliveredCaller, err)
	}
	skipsDrafts := strings.Contains(string(body), "github.event.pull_request.draft == false")
	hasTrigger := regexp.MustCompile(`(?m)^\s*types:.*ready_for_review`).MatchString(string(caller))

	switch {
	case skipsDrafts && !hasTrigger:
		t.Error("llz-terraform.yml skips DRAFT pull requests, but terraform.yml does not list " +
			"`ready_for_review` in its pull_request trigger types. Marking a PR ready for review then " +
			"fires no event, so the plan never runs and an operator has to push an empty commit to get " +
			"one — with nothing saying why. Add ready_for_review to the trigger types.")
	case !skipsDrafts && hasTrigger:
		t.Error("terraform.yml lists `ready_for_review` but nothing gates on draft any more — " +
			"either the skip was removed and this trigger type is now dead weight, or the gate moved " +
			"and this test is no longer watching it")
	case !skipsDrafts && !hasTrigger:
		t.Error("the draft skip is gone from llz-terraform.yml. It is what keeps the state-writing " +
			"Plan Cluster job off the release-e2e PR-gate probe's throwaway PR, which otherwise races " +
			"the provision apply on the same tfstate — see pr_gates.go's header.")
	}
}

// TestPinInTriggerImpliesImportIsPathGated couples the SECOND pair of halves
// that a copier upgrade moves by different rules — and the pair whose broken
// combination is silent rather than merely annoying.
//
// terraform.yml (`merge`) lists .copier-answers.yml in its pull_request paths so
// a pin-only upgrade PR reaches `repo-readiness`, which is where a newly
// mandatory secret gets caught. A paths: filter selects the WORKFLOW, not a job,
// so that entry also selects `Plan Cluster (PR)` — whose tf-import step WRITES
// cluster/<deployment>/terraform.tfstate against no lock. llz-terraform.yml
// (`managed`) is what keeps that safe, by gating the step on changed-paths.
//
// EITHER HALF ALONE IS WRONG, IN OPPOSITE DIRECTIONS. Pin listed + import
// ungated is the hazard restored: every automated upgrade PR takes an
// unserialized write at a bot's cadence rather than a human's. Import gated + pin
// unlisted is merely the old gap back, with the machinery to close it sitting
// unused. Neither shows up as a failure anywhere else — the first looks like a
// normal green PR right up until it races an apply.
func TestPinInTriggerImpliesImportIsPathGated(t *testing.T) {
	body, err := os.ReadFile(deliveredPipeline)
	if err != nil {
		t.Fatalf("read %s: %v", deliveredPipeline, err)
	}
	caller, err := os.ReadFile(deliveredCaller)
	if err != nil {
		t.Fatalf("read %s: %v", deliveredCaller, err)
	}

	pinListed := regexp.MustCompile(`(?m)^\s*-\s*'\.copier-answers\.yml'`).MatchString(string(caller))

	// Read the REAL step, not a substring that happens to appear in a comment:
	// find the tf-import step and require the `if:` on changed-paths inside it.
	// A test that grepped the whole file for the two strings would pass on the
	// prose above them, which is exactly how a gate stops watching anything.
	importStep := regexp.MustCompile(
		`(?s)- name: Import VPC and subnet if not in state\n(.*?)\n      - name: `,
	).FindStringSubmatch(string(body))
	if importStep == nil {
		t.Fatalf("no `Import VPC and subnet if not in state` step in %s — it was renamed or removed, "+
			"and this test would otherwise pass having read nothing. If the state write is genuinely "+
			"gone, delete this gate and say so; if it moved, re-point the pattern.", deliveredPipeline)
	}
	importGated := strings.Contains(importStep[1], "needs.changed-paths.outputs.terraform == 'true'")

	// And the job that produces the output must exist, or the `if:` above is a
	// reference to nothing — which GitHub evaluates to empty, i.e. never equal to
	// 'true', i.e. an import that silently stops running on EVERY PR.
	if importGated && !regexp.MustCompile(`(?m)^  changed-paths:`).MatchString(string(body)) {
		t.Error("the tf-import step gates on needs.changed-paths.outputs.terraform, but there is no " +
			"`changed-paths:` job to produce it. That expression evaluates to empty on every PR, so the " +
			"import never runs and every plan silently reports a pre-existing VPC as 'to be created'.")
	}

	switch {
	case pinListed && !importGated:
		t.Error("terraform.yml lists '.copier-answers.yml' in its pull_request paths, but " +
			"llz-terraform.yml's tf-import step is no longer gated on changed-paths. Every automated " +
			"template-upgrade PR now selects Plan Cluster (PR) and writes cluster/<deployment>/" +
			"terraform.tfstate with nothing serializing it against a concurrent apply — the exact " +
			"hazard the pin was removed from this filter to avoid. Restore the step's `if:`, or drop " +
			"the pin from the filter.")
	case !pinListed && importGated:
		t.Error("llz-terraform.yml gates tf-import on changed-paths — the mechanism that makes the " +
			"template pin safe to watch — but terraform.yml no longer lists '.copier-answers.yml' in " +
			"its pull_request paths. A pin-only upgrade PR therefore runs no repo-readiness, which is " +
			"where a newly mandatory secret is caught (v0.0.42 / TF_STATE_ENCRYPTION_PASSPHRASE). " +
			"Either add the pin back or remove the now-pointless gate.")
	}
}

// TestBuildArgvFieldsAreDeclaredInputsOfTerraformYml feeds the producer's REAL
// output into the consumer's REAL declaration, rather than restating either.
//
// `llz build` dispatches terraform.yml by writing `--field <name>=<value>`. Every
// one of those names is an input terraform.yml has to declare, and the two live
// in different languages in different trees with nothing but this test between
// them.
//
// THE FAILURE IS ASYMMETRIC, WHICH IS WHY IT NEEDS A GATE. `gh workflow run`
// rejects a field the workflow does not declare, so a typo surfaces loudly at
// dispatch. But an input that is RENAMED — `assert_loki` to `assert_invariants`,
// exactly what this change did — leaves the workflow declaring a name nothing
// sends and `llz build` sending a name nothing reads. `gh` would reject the stale
// field, but only for the caller that still passes it; the far worse shape is the
// reverse, where the field is dropped from argv and the flag silently stops
// turning the assertions on. The run then converges, asserts nothing, and goes
// green — the precise "green having examined nothing" this repo keeps paying for.
func TestBuildArgvFieldsAreDeclaredInputsOfTerraformYml(t *testing.T) {
	caller, err := os.ReadFile(deliveredCaller)
	if err != nil {
		t.Fatalf("read %s: %v", deliveredCaller, err)
	}
	// The workflow_dispatch input block: names at six-space indent under `inputs:`.
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^      ([a-z_]+):\s*$`).FindAllStringSubmatch(string(caller), -1) {
		declared[m[1]] = true
	}
	if len(declared) < 3 {
		t.Fatalf("found only %d declared input(s) in %s — the extractor is broken, so this "+
			"test would pass having compared nothing", len(declared), deliveredCaller)
	}

	// Both modes, because the assert field only appears in one of them.
	for _, assertInvariants := range []bool{false, true} {
		argv := buildArgv("lab", assertInvariants)
		fields := 0
		for i, a := range argv {
			if a != "--field" || i+1 >= len(argv) {
				continue
			}
			name, _, ok := strings.Cut(argv[i+1], "=")
			if !ok {
				t.Errorf("argv field %q is not name=value", argv[i+1])
				continue
			}
			fields++
			if !declared[name] {
				t.Errorf("`llz build` dispatches --field %s=…, but terraform.yml declares no such "+
					"workflow_dispatch input (declared: %v).\n"+
					"\tEither the input was renamed and buildArgv still sends the old name — in which "+
					"case gh rejects the dispatch — or buildArgv invented a name nothing reads, in "+
					"which case the run goes green having done none of what the field asked for.",
					name, sortedKeys(declared))
			}
		}
		if fields == 0 {
			t.Fatal("buildArgv emitted no --field pairs; the extractor is broken, not the argv")
		}
	}

	// And the direction that a name check alone cannot see: the flag must actually
	// change the argv. A --assert-invariants that quietly emits nothing is the
	// silent-green case above, and it would satisfy every assertion so far.
	off, on := buildArgv("lab", false), buildArgv("lab", true)
	if len(on) <= len(off) {
		t.Error("--assert-invariants added no field to the dispatch, so the flag is inert: the run " +
			"would converge and assert nothing while reporting success")
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── Gate 3: which delivered entry points are exercised at all ────────────────

// exercisedEntryPoints records, per delivered entry-point workflow, WHY it is
// considered covered — or, for an exclusion, why it is knowingly not.
//
// An "entry point" is a delivered workflow with a real trigger (push /
// pull_request / schedule / workflow_dispatch). A `workflow_call`-only body is
// not one: it runs whenever a caller runs, so it inherits its caller's coverage.
//
// THE EXCLUSIONS ARE THE POINT OF THE GATE, not a loophole in it. Nothing here
// can execute these workflows; what it can do is refuse to let the set of
// unexercised ones grow silently. Adding a delivered workflow now forces one of
// two decisions, in writing: drive it from the release lane, or say here why not.
// The template's release-e2e lane drives exactly ONE instance workflow —
// terraform.yml — which is a fact worth having written down somewhere a reviewer
// meets, because it was not obvious to anyone until a rotation workflow turned
// out to have been dead for months.
var exercisedEntryPoints = map[string]string{
	"terraform.yml": "DRIVEN: release-e2e dispatches it (apply + destroy) and the PR-gate probe " +
		"opens a pull request against it — the only delivered workflow the lane actually runs.",

	// ── Knowingly unexercised, with the risk each one carries ───────────────
	"scheduled-checks.yml": "NOT DRIVEN: cron-only drift/health reporting. Its body (llz-scheduled-checks.yml) " +
		"is workflow_call and its verbs are unit-tested; what is unproven is the caller stub — the exact " +
		"surface where the rotation workflow's startup_failure lived. reusable-workflow-caller-permissions " +
		"gates that specific class statically.",
	"secret-rotation.yml": "NOT DRIVEN: rotating real credentials on a throwaway instance would mint and revoke " +
		"account-scoped Linode PATs on a shared account. THIS IS THE ONE THAT WAS DEAD FOR MONTHS " +
		"(caller job missing id-token: write -> startup_failure, no jobs, no annotations, no notification). " +
		"Now covered statically by reusable-workflow-caller-permissions.",
	"cluster-health.yml": "NOT DRIVEN as a stub: the former e2e `validate` job that dispatched it was folded into " +
		"the converge gate, so llz-cluster-health.yml's BODY runs every e2e via llz-bootstrap-openbao. " +
		"Only the dispatch stub is unexercised.",
	"bootstrap-openbao.yml": "NOT DRIVEN as a stub: llz-bootstrap-openbao.yml's body runs on every e2e as part of " +
		"terraform.yml's apply chain. Only the standalone-retry dispatch stub is unexercised.",
	"breakglass-openbao.yml": "NOT DRIVEN: an emergency handle for a wedged OpenBao. Running it in the lane would " +
		"regenerate a root token on every release for no signal.",
	"wedge-gameday.yml": "NOT DRIVEN by the release lane: it deliberately INJECTS wedges, so running it inside the " +
		"gate would fail the gate. It is exercised by hand during gameday exercises.",
	"promote.yml": "NOT DRIVEN: rendered per-instance from promotion_rank and a no-op for the <2 ranked " +
		"deployments the e2e instance has. promote-pipeline-drift checks it is in sync on every PR.",
	"scheduled-apply.yml": "NOT DRIVEN: it dispatches terraform.yml, which the release lane already drives " +
		"directly — running it here would be the same apply through one more hop, on a schedule the lane " +
		"does not have. What is unproven is the stub's own trigger surface and the armed/verdict gating; " +
		"reusable-workflow-caller-permissions covers the startup_failure class statically, and the " +
		"llz build --watch fail-closed arms are unit-tested in build_watch_wait_test.go.",
	"template-upgrade.yml": "NOT DRIVEN by the release lane: it upgrades an instance to the LATEST published " +
		"release, and the lane's instance is scaffolded from the commit under test — so the workflow would " +
		"either no-op or drag the fixture onto a different version mid-run. The `llz upgrade` it wraps is " +
		"covered by `llz ci upgrade-test`, which performs a real upgrade across the last 3 releases on " +
		"every lint run.",
}

// workflowCallOnly matches a delivered body that is only ever invoked by another
// workflow, so it carries no coverage question of its own.
func workflowCallOnly(body string) bool {
	triggers := regexp.MustCompile(`(?m)^  (push|pull_request|schedule|workflow_dispatch|workflow_call):`)
	found := map[string]bool{}
	for _, m := range triggers.FindAllStringSubmatch(body, -1) {
		found[m[1]] = true
	}
	return found["workflow_call"] && len(found) == 1
}

func TestEveryDeliveredEntryPointIsExercisedOrExcused(t *testing.T) {
	dir := "../../../instance-template/.github/workflows"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var entryPoints []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := filepath.Ext(e.Name()); ext != ".yml" && ext != ".yaml" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if workflowCallOnly(string(raw)) {
			continue // inherits its callers' coverage
		}
		entryPoints = append(entryPoints, e.Name())
	}
	sort.Strings(entryPoints)

	// Fail closed on vacuity, the same way the sibling gate does: an extractor
	// that stopped seeing workflows would otherwise report a clean tree.
	if len(entryPoints) < 5 {
		t.Fatalf("found only %d delivered entry-point workflow(s) (%v) — the classifier is broken, "+
			"not the tree", len(entryPoints), entryPoints)
	}

	for _, name := range entryPoints {
		if _, ok := exercisedEntryPoints[name]; !ok {
			t.Errorf("delivered workflow %q has a real trigger but is not listed in exercisedEntryPoints. "+
				"Nothing in this repo runs it, so nothing would notice it breaking — the way "+
				"secret-rotation.yml produced startup_failure nightly for months. Either drive it from the "+
				"release-e2e lane, or add it with a one-line note saying why not and what covers it instead.",
				name)
		}
	}

	// And the reverse: a stale entry would quietly excuse a workflow that no
	// longer exists, so the list cannot rot into a list of names nobody checks.
	for name := range exercisedEntryPoints {
		if !slices.Contains(entryPoints, name) {
			t.Errorf("exercisedEntryPoints names %q, which is not a delivered entry-point workflow "+
				"(deleted, renamed, or now workflow_call-only) — drop the entry", name)
		}
	}

	driven := 0
	for _, name := range entryPoints {
		if strings.HasPrefix(exercisedEntryPoints[name], "DRIVEN") {
			driven++
		}
	}
	t.Logf("%d delivered entry point(s); %d driven by the release lane, %d excused",
		len(entryPoints), driven, len(entryPoints)-driven)
}

// ── Gate 4: the probe's check names vs the delivered jobs' own ───────────────

// TestPRGateCheckNamesMatchTheDeliveredJobs couples the last restated list in
// this PR's new machinery.
//
// DefaultPRGateChecks names the two jobs `llz ci assert-instance-pr-gates` waits
// for, and llz-terraform.yml names them again in its `name:` fields. Rename a job
// there and nothing in this repo notices: the template stays green, and the
// mismatch surfaces fifteen minutes into the next release-e2e as "the gates never
// ran — either the paths: filter no longer covers AGENTS.md, or the jobs were
// removed or renamed". That diagnosis lists the right cause third, behind two
// wrong ones, for a rename a static gate can see instantly. Every other split
// contract in this branch got a coupling test; this is the one that did not.
func TestPRGateCheckNamesMatchTheDeliveredJobs(t *testing.T) {
	raw, err := os.ReadFile(deliveredPipeline)
	if err != nil {
		t.Fatalf("read %s: %v", deliveredPipeline, err)
	}
	// Job `name:` values, at job depth (4 spaces) inside the pipeline.
	names := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^    name: (.+)$`).FindAllStringSubmatch(string(raw), -1) {
		names[strings.TrimSpace(strings.Trim(m[1], `"'`))] = true
	}
	if len(names) < 5 {
		t.Fatalf("found only %d job name(s) in %s — the extractor is broken, so this gate would pass "+
			"having compared nothing", len(names), deliveredPipeline)
	}
	for _, want := range releasepublish.DefaultPRGateChecks {
		if !names[want] {
			var have []string
			for n := range names {
				have = append(have, n)
			}
			sort.Strings(have)
			t.Errorf("the PR-gate probe waits for a check called %q, but no job in %s is named that.\n"+
				"A release-e2e run would report it as \"the gates never ran\" and send an operator to the "+
				"paths: filter. Jobs actually named there: %s",
				want, deliveredPipeline, strings.Join(have, ", "))
		}
	}
}
