package promote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The vendored-body form every ADR-0003 instance renders (see localTerraformUses)
// and the legacy cross-repo pin an older instance still carries.
const testLegacyUses = "akamai-consulting/lke-landing-zone/.github/workflows/llz-terraform.yml@v1.2.3"

func testCaller() promoCaller {
	return promoCaller{uses: localTerraformUses, instanceRepo: "myorg/my-instance"}
}

// testStub renders the minimal caller-stub YAML callerFromWorkflow parses, in the
// same shape the copier-delivered terraform.yml has.
func testStub(uses string) string {
	return "jobs:\n  call:\n    uses: " + uses + "\n    with:\n" +
		"      instance_repo: myorg/my-instance\n"
}

func TestRenderPromoteWorkflowChainsNeeds(t *testing.T) {
	out := renderPromoteWorkflow(testCaller(), []promoStage{
		{name: "dev", rank: 1},
		{name: "staging", rank: 2},
		{name: "prod", rank: 3},
	})

	// Title lists the stages in order.
	if !strings.Contains(out, "name: Promote (dev → staging → prod)") {
		t.Errorf("missing/incorrect workflow title:\n%s", out)
	}
	// The first stage chains from the PREFLIGHT (not from nothing — that is what
	// left a dispatched pipeline ungated); later stages chain to the previous one.
	devIdx := strings.Index(out, "\n  dev:\n")
	stagingIdx := strings.Index(out, "\n  staging:\n")
	prodIdx := strings.Index(out, "\n  prod:\n")
	if devIdx < 0 || stagingIdx < 0 || prodIdx < 0 {
		t.Fatalf("missing a stage job:\n%s", out)
	}
	if devIdx > stagingIdx || stagingIdx > prodIdx {
		t.Errorf("jobs not emitted in promotion order")
	}
	devBlock := out[devIdx:stagingIdx]
	if !strings.Contains(devBlock, "needs: "+preflightJob) {
		t.Errorf("entry stage dev must chain from the preflight:\n%s", devBlock)
	}
	if !strings.Contains(out[stagingIdx:prodIdx], "needs: dev") {
		t.Errorf("staging must `needs: dev`")
	}
	if !strings.Contains(out[prodIdx:], "needs: staging") {
		t.Errorf("prod must `needs: staging`")
	}

	// Each stage calls the vendored local body and carries the apply selectors.
	if strings.Count(out, "uses: "+localTerraformUses) != 3 {
		t.Errorf("expected the local vendored-body uses: on all 3 stages")
	}
	// The template pin is resolved at runtime (pinnedTemplateRef), so a stage must
	// NOT carry a template-ref input — that is what made every upgrade churn
	// promote.yml three times over.
	if strings.Contains(out, "template-ref") {
		t.Errorf("promote stages must not render a template-ref input:\n%s", out)
	}
	if strings.Count(out, "instance_repo: myorg/my-instance") != 3 {
		t.Errorf("instance_repo not rendered on every stage")
	}
	if strings.Count(out, "region: dev")+strings.Count(out, "region: staging")+strings.Count(out, "region: prod") != 3 {
		t.Errorf("per-stage region: not rendered")
	}
	if !strings.Contains(out, "module: ${{ inputs.module || 'all' }}") {
		t.Errorf("module input wiring missing")
	}
	if strings.Count(out, "secrets: inherit") != 3 {
		t.Errorf("secrets: inherit not on every stage")
	}
	if !strings.Contains(out, "GENERATED") {
		t.Errorf("generated-file header missing")
	}
}

func TestCallerFromWorkflow(t *testing.T) {
	dir := t.TempDir()

	// The ADR-0003 local form: uses is the vendored body, plus instance_repo.
	local := filepath.Join(dir, "terraform.yml")
	mustWrite(t, local, testStub(localTerraformUses))
	c, ok := callerFromWorkflow(local)
	if !ok {
		t.Fatal("expected ok for a rendered local-uses caller stub")
	}
	if c.uses != localTerraformUses || c.instanceRepo != "myorg/my-instance" {
		t.Errorf("extracted %+v", c)
	}

	// A legacy instance's cross-repo pin is preserved verbatim.
	legacy := filepath.Join(dir, "legacy.yml")
	mustWrite(t, legacy, "jobs:\n  call:\n    uses: "+testLegacyUses+"\n    with:\n      instance_repo: myorg/my-instance\n")
	c, ok = callerFromWorkflow(legacy)
	if !ok {
		t.Fatal("expected ok for a legacy cross-repo caller stub")
	}
	if c.uses != testLegacyUses {
		t.Errorf("legacy extracted %+v", c)
	}

	// An unrendered copier-token template must be rejected (no concrete pin) —
	// including the local-uses form, whose uses: line is a literal that exists in
	// the un-rendered template too (only the with: pins carry tokens there).
	tok := filepath.Join(dir, "tmpl.yml")
	mustWrite(t, tok, "    uses: <@ upstream_org @>/lke-landing-zone/.github/workflows/llz-terraform.yml@<@ llz_version @>\n")
	if _, ok := callerFromWorkflow(tok); ok {
		t.Errorf("copier-token template must not be accepted as a concrete caller")
	}
	tokLocal := filepath.Join(dir, "tmpl-local.yml")
	mustWrite(t, tokLocal, "jobs:\n  call:\n    uses: "+localTerraformUses+"\n    with:\n      instance_repo: <@ instance_repo @>\n")
	if _, ok := callerFromWorkflow(tokLocal); ok {
		t.Errorf("un-rendered local-uses template must not be accepted as a concrete caller")
	}

	if _, ok := callerFromWorkflow(filepath.Join(dir, "absent.yml")); ok {
		t.Errorf("absent file must return ok=false")
	}
}

func TestSyncPromoteWorkflowRoundTrip(t *testing.T) {
	root := t.TempDir()
	chdir(t, root)

	// A rendered instance: cluster tfvars with ranks + a concrete terraform.yml to
	// lift the pin from (no promote.yml yet).
	writeCluster(t, "tf", map[string]string{
		"dev.tfvars":     "promotion_rank = 1\n",
		"staging.tfvars": "promotion_rank = 2\n",
		"prod.tfvars":    "promotion_rank = 3\n",
		"lab.tfvars":     "region = \"us-x\"\n", // unranked → excluded
	})
	if err := os.MkdirAll(filepath.Join(".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(".github", "workflows", "terraform.yml"), testStub(localTerraformUses))

	plan, err := PlanWorkflow(testDeps(), "tf", "")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.Changed {
		t.Fatal("expected the first generation to report changed")
	}
	// applyPlan is what cmd/llz does. The package plans and does not write (see
	// TestPackageContainsNoWritePath), so the round trip is exercised by doing the
	// write here — the same two lines the command runs.
	applyPlan(t, plan)

	got, err := os.ReadFile(filepath.Join(".github", "workflows", "promote.yml"))
	if err != nil {
		t.Fatalf("promote.yml not written: %v", err)
	}
	want := renderPromoteWorkflow(testCaller(), []promoStage{{"dev", 1}, {"staging", 2}, {"prod", 3}})
	if string(got) != want {
		t.Errorf("written content != rendered content")
	}

	// Planning against the freshly-written file: no drift.
	if p2, err := PlanWorkflow(testDeps(), "tf", ""); err != nil || p2.Changed {
		t.Errorf("plan after write = changed %v, err %v; want false,nil", p2.Changed, err)
	}

	// Re-rank: insert a stage. The plan must now report drift; applying reconciles it.
	mustWrite(t, filepath.Join("tf", "cluster", "canary.tfvars"), "promotion_rank = 4\n")
	p3, err := PlanWorkflow(testDeps(), "tf", "")
	if err != nil || !p3.Changed {
		t.Errorf("plan after re-rank = changed %v, err %v; want true,nil", p3.Changed, err)
	}
	applyPlan(t, p3)
	if p4, _ := PlanWorkflow(testDeps(), "tf", ""); p4.Changed {
		t.Errorf("still drifting after re-apply")
	}
}

// applyPlan mirrors cmd/llz's syncPromoteWorkflow write half, so this package's
// tests can exercise the full round trip without this package containing a write.
func applyPlan(t *testing.T, p Plan) {
	t.Helper()
	if !p.Changed {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Path, []byte(p.Content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSyncPromoteWorkflowSkips(t *testing.T) {
	root := t.TempDir()
	chdir(t, root)

	// Fewer than two ranked stages — not a pipeline; nothing written.
	writeCluster(t, "tf", map[string]string{
		"dev.tfvars": "promotion_rank = 1\n",
		"lab.tfvars": "region = \"us-x\"\n",
	})
	if plan, err := PlanWorkflow(testDeps(), "tf", ""); err != nil || plan.Changed {
		t.Errorf("one ranked stage: changed %v err %v; want false,nil", plan.Changed, err)
	}
	if _, err := os.Stat(filepath.Join(".github", "workflows", "promote.yml")); !os.IsNotExist(err) {
		t.Errorf("promote.yml should not exist for a sub-pipeline rank set")
	}

	// Template-repo layout (relPrefix set): generation is skipped entirely.
	if plan, err := PlanWorkflow(testDeps(), "tf", "instance-template/"); err != nil || plan.Changed {
		t.Errorf("template layout: changed %v err %v; want false,nil", plan.Changed, err)
	}
}
