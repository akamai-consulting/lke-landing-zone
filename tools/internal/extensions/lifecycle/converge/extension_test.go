package converge

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("converge does not validate: %v", err)
		}
	}
}

// THE ACID TEST'S ASSERTION. `ci_health.go` fused the converge ACTION with the
// health PREDICATE, and the catalog's whole claim about this extension was that
// the model would force them apart. Pin the separation: if `health` ever gains
// cluster-write, the fusion is back and the declaration has stopped meaning
// anything.
//
// This is not hypothetical. `converge` nudges Argo Applications (a patch) and
// strips oversized last-applied-configuration annotations off CRDs when a sync
// hits the 256KB limit — cluster writes performed from inside what reads like a
// health check. A reviewer running `llz ci health --wait` would reasonably assume
// it observes; the grant line is the correction.
func TestActionAndPredicateStaySeparate(t *testing.T) {
	byName := map[string]extension.Binding{}
	for _, b := range Extension().Bindings {
		byName[b.Name] = b
	}

	drive, ok := byName["drive"]
	if !ok {
		t.Fatal("no binding named \"drive\" — the converge ACTION must be declared separately " +
			"from the health predicate; that split is this extension's entire finding")
	}
	if drive.Kind != extension.Transition {
		t.Errorf("drive kind = %s, want transition — it patches Argo Applications and strips "+
			"CRD annotations, so it moves the world", drive.Kind)
	}
	if !hasGrant(drive, extension.ClusterWrite) {
		t.Error("drive dropped cluster-write — nudge-argo still PATCHes Applications and the " +
			"256KB annotation stripper still writes, so dropping it hides both")
	}

	for _, name := range []string{"health", "health-incluster"} {
		b, ok := byName[name]
		if !ok {
			t.Errorf("no binding named %q", name)
			continue
		}
		if b.Kind != extension.Assertion {
			t.Errorf("%s kind = %s, want assertion — it reports a verdict and a second run "+
				"changes nothing", name, b.Kind)
		}
		if hasGrant(b, extension.ClusterWrite) {
			t.Errorf("%s gained cluster-write — the predicate half must not mutate what it "+
				"measures. If it now does, that belongs on the `drive` transition", name)
		}
	}
}

// Both verdict paths bind the same state and hold the same grants; what differs is
// REACH (kubectl vs internal/kube on the pod ServiceAccount). Naming them
// separately is what makes "two ways to compute this, and they must agree" visible
// in the declaration. Assert they have not been collapsed into one.
func TestBothVerdictPathsRemainDeclared(t *testing.T) {
	var assertions int
	for _, b := range Extension().Bindings {
		if b.Kind == extension.Assertion {
			assertions++
			if b.State != extension.Converged {
				t.Errorf("%s: state = %s, want converged", b.Name, b.State)
			}
		}
	}
	if assertions != 2 {
		t.Errorf("want two assertion bindings (kubectl and kubectl-free), got %d — collapsing "+
			"them hides that a distroless in-cluster runner computes this verdict too", assertions)
	}
}

func hasGrant(b extension.Binding, want extension.Grant) bool {
	for _, g := range b.Grants {
		if g == want {
			return true
		}
	}
	return false
}
