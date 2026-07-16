package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The vendored-body form every ADR-0003 instance renders (see localTerraformUses)
// and the legacy cross-repo pin an older instance still carries.
const (
	testDepName    = "akamai-consulting/lke-landing-zone"
	testLegacyUses = "akamai-consulting/lke-landing-zone/.github/workflows/llz-terraform.yml@v1.2.3"
)

func testCaller() promoCaller {
	return promoCaller{uses: localTerraformUses, instanceRepo: "myorg/my-instance", templateRef: "v1.2.3", depName: testDepName}
}

// testStub renders the minimal caller-stub YAML callerFromWorkflow parses, in the
// same shape the copier-delivered terraform.yml has.
func testStub(uses string) string {
	return "jobs:\n  call:\n    uses: " + uses + "\n    with:\n" +
		"      instance_repo: myorg/my-instance\n" +
		"      # renovate: datasource=github-tags depName=" + testDepName + "\n" +
		"      template-ref: v1.2.3\n"
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
	// First stage has NO needs; later stages chain to the previous one.
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
	if strings.Contains(devBlock, "needs:") {
		t.Errorf("entry stage dev must not declare needs:\n%s", devBlock)
	}
	if !strings.Contains(out[stagingIdx:prodIdx], "needs: dev") {
		t.Errorf("staging must `needs: dev`")
	}
	if !strings.Contains(out[prodIdx:], "needs: staging") {
		t.Errorf("prod must `needs: staging`")
	}

	// Each stage carries the preserved pin + the apply selectors.
	if strings.Count(out, "uses: "+testUses) != 3 {
		t.Errorf("expected the pin reused on all 3 stages")
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
	concrete := filepath.Join(dir, "terraform.yml")
	mustWrite(t, concrete, "jobs:\n  call:\n    uses: "+testUses+"\n    with:\n      instance_repo: myorg/my-instance\n      template-ref: v1.2.3\n")
	c, ok := callerFromWorkflow(concrete)
	if !ok {
		t.Fatal("expected ok for a concrete caller stub")
	}
	if c.uses != testUses || c.instanceRepo != "myorg/my-instance" || c.templateRef != "v1.2.3" {
		t.Errorf("extracted %+v", c)
	}

	// An unrendered copier-token template must be rejected (no concrete pin).
	tok := filepath.Join(dir, "tmpl.yml")
	mustWrite(t, tok, "    uses: <@ upstream_org @>/lke-landing-zone/.github/workflows/llz-terraform.yml@<@ llz_version @>\n")
	if _, ok := callerFromWorkflow(tok); ok {
		t.Errorf("copier-token template must not be accepted as a concrete caller")
	}

	if _, ok := callerFromWorkflow(filepath.Join(dir, "absent.yml")); ok {
		t.Errorf("absent file must return ok=false")
	}
}

func TestDepNameFromUses(t *testing.T) {
	if got := depNameFromUses(testUses); got != "akamai-consulting/lke-landing-zone" {
		t.Errorf("depNameFromUses = %q", got)
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
	mustWrite(t, filepath.Join(".github", "workflows", "terraform.yml"),
		"jobs:\n  call:\n    uses: "+testUses+"\n    with:\n      instance_repo: myorg/my-instance\n      template-ref: v1.2.3\n")

	changed, err := syncPromoteWorkflow("tf", "", false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !changed {
		t.Fatal("expected the first generation to report changed")
	}
	got, err := os.ReadFile(filepath.Join(".github", "workflows", "promote.yml"))
	if err != nil {
		t.Fatalf("promote.yml not written: %v", err)
	}
	want := renderPromoteWorkflow(testCaller(), []promoStage{{"dev", 1}, {"staging", 2}, {"prod", 3}})
	if string(got) != want {
		t.Errorf("written content != rendered content")
	}

	// --check on the freshly-written file: no drift.
	if drift, err := syncPromoteWorkflow("tf", "", true); err != nil || drift {
		t.Errorf("check after write = drift %v, err %v; want false,nil", drift, err)
	}

	// Re-rank: insert a stage. --check must now report drift; a write reconciles it.
	mustWrite(t, filepath.Join("tf", "cluster", "canary.tfvars"), "promotion_rank = 4\n")
	if drift, err := syncPromoteWorkflow("tf", "", true); err != nil || !drift {
		t.Errorf("check after re-rank = drift %v, err %v; want true,nil", drift, err)
	}
	if _, err := syncPromoteWorkflow("tf", "", false); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if drift, _ := syncPromoteWorkflow("tf", "", true); drift {
		t.Errorf("still drifting after re-sync")
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
	if changed, err := syncPromoteWorkflow("tf", "", false); err != nil || changed {
		t.Errorf("one ranked stage: changed %v err %v; want false,nil", changed, err)
	}
	if _, err := os.Stat(filepath.Join(".github", "workflows", "promote.yml")); !os.IsNotExist(err) {
		t.Errorf("promote.yml should not exist for a sub-pipeline rank set")
	}

	// Template-repo layout (relPrefix set): generation is skipped entirely.
	if changed, err := syncPromoteWorkflow("tf", "instance-template/", false); err != nil || changed {
		t.Errorf("template layout: changed %v err %v; want false,nil", changed, err)
	}
}
