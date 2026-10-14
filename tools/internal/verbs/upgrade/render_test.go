package upgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envdef"
)

// writeInstanceFile writes rel under dir, creating parents.
func writeInstanceFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRenderableInstance builds the smallest tree renderAfter will act on:
// copier answers pinned to `pin`, the tfvars example each root renders from, and
// one authored env.
func newRenderableInstance(t *testing.T, pin string) string {
	t.Helper()
	dir := t.TempDir()
	writeInstanceFile(t, dir, ".copier-answers.yml",
		"upstream_org: akamai-consulting\ninstance_repo: my-org/platform-support\nllz_version: "+pin+"\nopenbao_team: ops\n")
	writeInstanceFile(t, dir, "terraform-iac-bootstrap/cluster/terraform.tfvars.example",
		"cluster_label = \"x\"\nk8s_version = \"v1.33.6+lke7\"\nnode_type  = \"g8-dedicated-8-4\"\nnode_count = 5\n")
	if _, _, err := envdef.EnsureLandingZone(dir, ""); err != nil {
		t.Fatalf("envdef.EnsureLandingZone: %v", err)
	}
	if err := envdef.WriteEnvDefinition(filepath.Join(dir, "environments", "lab.yaml"), "lab",
		envdef.Opts{Region: "us-sea", ObjCluster: "us-sea-1", NodeCount: "3"}, "platform-support"); err != nil {
		t.Fatalf("envdef.WriteEnvDefinition: %v", err)
	}
	handVersionToLinode(t, filepath.Join(dir, "landingzone.yaml"))
	return dir
}

// handVersionToLinode writes `manageAplVersion: false` into a scaffolded root.
//
// WHY THE UPGRADE FIXTURES NEED A STANCE AT ALL. The field defaults ON, and an
// owned deployment's aplChartVersion is what DEPLOYS — so sweepAplPins refuses to
// touch it, by design. A fixture that said nothing would therefore exercise the
// ownership guard on every assertion instead of the pin sweep those tests are
// about. Ownership itself is covered in aplpin_owned_test.go.
//
// Spliced into the scaffold's FLOW mapping (`bootstrap: { managedAppPlatform:
// true }`) rather than appended as a block key, because a second `bootstrap:` at
// the same level is a duplicate key and yaml.v3 rejects the document — which
// would fail these tests for a reason that has nothing to do with what they test.
func handVersionToLinode(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read scaffolded root: %v", err)
	}
	s := string(b)
	if strings.Contains(s, "manageAplVersion") {
		return
	}
	const flow = "bootstrap: { managedAppPlatform: true }"
	if !strings.Contains(s, flow) {
		t.Fatalf("scaffolded root no longer carries %q — this helper needs updating:\n%s", flow, s)
	}
	s = strings.Replace(s, flow, "bootstrap: { managedAppPlatform: true, manageAplVersion: false }", 1)
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatalf("write scaffolded root: %v", err)
	}
}

// The whole reason the render moved into `llz upgrade`: the pin copier rewrites is
// what the committed apl-values `?ref=` resolves to, so every kustomization is
// stale by construction until a render runs. Assert the emitted refs carry the
// NEW pin — not that a render merely happened.
func TestRenderAfterUpgradeRepinsAplValues(t *testing.T) {
	dir := newRenderableInstance(t, "v0.4.0")
	t.Chdir(dir)

	if err := renderAfter(false); err != nil {
		t.Fatalf("renderAfter: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "apl-values", "lab", "manifest", "kustomization.yaml"))
	if err != nil {
		t.Fatalf("the manifest overlay should exist after a render: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, "?ref=v0.4.0") {
		t.Errorf("rendered refs should carry the new pin v0.4.0:\n%s", got)
	}
	if strings.Contains(got, "?ref=v0.3.0") {
		t.Errorf("rendered refs still carry an old pin:\n%s", got)
	}
}

// An instance that never adopted the spec must still upgrade — the same
// InstancePresent guard stepRenderFresh uses, so the two agree about what "there
// is nothing to render" means.
func TestRenderAfterUpgradeSkipsPreSpecInstance(t *testing.T) {
	dir := t.TempDir()
	writeInstanceFile(t, dir, ".copier-answers.yml", "llz_version: v0.4.0\n")
	t.Chdir(dir)

	if err := renderAfter(false); err != nil {
		t.Fatalf("a pre-spec instance should render nothing and pass, got %v", err)
	}
}

// A render failure leaves a genuinely half-upgraded tree, so the message must say
// so rather than surfacing a bare spec error the operator cannot place.
func TestRenderAfterUpgradeExplainsAHalfUpgradedTree(t *testing.T) {
	dir := newRenderableInstance(t, "v0.4.0")
	// Invalidate the spec: a region the validator rejects.
	writeInstanceFile(t, dir, "environments/lab.yaml",
		"name: lab\ncluster:\n  region: \"\"\n  network: {}\n")
	t.Chdir(dir)

	err := renderAfter(false)
	if err == nil {
		t.Fatal("an invalid spec should fail the post-upgrade render")
	}
	for _, want := range []string{"OLD", "llz render"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should explain the half-upgraded tree and the remedy (missing %q):\n%v", want, err)
		}
	}
}
