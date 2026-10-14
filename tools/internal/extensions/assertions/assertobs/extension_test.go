package assertobs

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("assert-observability does not validate: %v", err)
		}
	}
}

// THE MUTATION MUST STAY VISIBLE. ci_readiness.go runs `kubectl rollout restart`
// to make Harbor pick up the obj-proxy CA — a cluster write reached from lanes
// whose names all begin `assert-`, in a file called "readiness". An operator
// running `llz ci wait-harbor` would not expect a Deployment restart.
//
// If this binding is ever dropped or downgraded to an assertion, the write becomes
// invisible again and the declaration starts lying about the most surprising thing
// this extension does.
func TestTheHarborRetrofitStaysDeclared(t *testing.T) {
	for _, b := range Extension().Bindings {
		if b.Name != "harbor-ca-retrofit" {
			continue
		}
		if b.Kind != extension.Transition {
			t.Errorf("kind = %s, want transition — it restarts Deployments", b.Kind)
		}
		if b.State != extension.Converged {
			t.Errorf("state = %s, want converged. Note `verified` will not accept a transition "+
				"at all (assert-network found that), and the retrofit is a step TOWARD "+
				"convergence rather than an attestation about it", b.State)
		}
		var writes bool
		for _, g := range b.Grants {
			if g == extension.ClusterWrite {
				writes = true
			}
		}
		if !writes {
			t.Error("dropped cluster-write — the retrofit still runs `kubectl rollout restart`, " +
				"so dropping the grant makes the declaration the thing that is wrong")
		}
		return
	}
	t.Fatal("no binding named \"harbor-ca-retrofit\" — the cluster write it performs would then " +
		"be undeclared, which is how it went unnoticed before this extraction")
}

// The observing lanes must not acquire the retrofit's grant by association.
func TestObservingLanesStayReadOnly(t *testing.T) {
	for _, b := range Extension().Bindings {
		if b.Name == "harbor-ca-retrofit" {
			continue
		}
		if b.Kind != extension.Assertion {
			t.Errorf("%s: kind = %s, want assertion", b.Name, b.Kind)
		}
		for _, g := range b.Grants {
			if g == extension.ClusterWrite || g == extension.CloudMutate || g == extension.SecretCustody {
				t.Errorf("%s holds %q — the read lanes must not inherit the retrofit's grant", b.Name, g)
			}
		}
	}
}
