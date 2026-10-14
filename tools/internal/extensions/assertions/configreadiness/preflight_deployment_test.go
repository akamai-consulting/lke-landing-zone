package configreadiness

// preflight_deployment_test.go — the flag the delivered workflow passes must
// exist, and must reach the resolver.
//
// THIS IS THE CONTRACT THAT BROKE. `instance-template/.github/workflows/
// llz-terraform.yml` runs `llz ci preflight --deployment "$REGION"`. The
// extraction that moved this command into configreadiness dropped both the flag
// and its ResolveDeploymentScope call, so apply-cluster died on
// `unknown flag: --deployment` before Terraform ran — in e2e, and in every
// adopter's instance. Nothing failed at PR time: the branch was green on all
// eleven checks.
//
// docs-guard now validates workflow invocations against the live cobra tree,
// which catches the flag going missing again from the OTHER side. This is the
// near side: the command's own package asserting the surface it publishes.

import (
	"strings"
	"testing"
)

// TestPreflightAcceptsTheFlagsTheDeliveredWorkflowPasses names them explicitly
// rather than parsing the YAML: this test must keep working inside an instance
// checkout, where the template's workflow is not present.
func TestPreflightAcceptsTheFlagsTheDeliveredWorkflowPasses(t *testing.T) {
	c := PreflightCmd()
	for _, name := range []string{"deployment", "fail-on-orphans", "orphan-threshold"} {
		if c.Flags().Lookup(name) == nil {
			t.Errorf("`llz ci preflight` has no --%s, but llz-terraform.yml passes it — "+
				"apply-cluster will die on `unknown flag` before Terraform runs", name)
		}
	}
}

// TestPreflightDeploymentFlagDocumentsTheCensusTrap. The flag's whole point is
// that a DEPLOYMENT name handed to a Linode-REGION filter matches no Volume, so
// the orphan census reads 0 however many are stranded. A help string that loses
// that turns the flag back into a mystery.
func TestPreflightDeploymentFlagDocumentsTheCensusTrap(t *testing.T) {
	f := PreflightCmd().Flags().Lookup("deployment")
	if f == nil {
		t.Fatal("no --deployment flag")
	}
	if !strings.Contains(f.Usage, "tfvars") {
		t.Errorf("usage should say where the region comes from, got %q", f.Usage)
	}
}
