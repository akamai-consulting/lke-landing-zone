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
