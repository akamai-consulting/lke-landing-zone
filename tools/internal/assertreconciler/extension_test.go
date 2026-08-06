package assertreconciler

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("assert-reconciler does not validate: %v", err)
		}
	}
}

// Only the second opt-in extension, and the first since import-brownfield. Pin it:
// an instance that runs no reconciler has nothing here to assert about, and a lane
// that always fails on such an instance is a lane operators learn to ignore —
// which is worse than no lane, because it also trains them to ignore the ones that
// mean something.
func TestAssertReconcilerIsOptIn(t *testing.T) {
	if Extension().Always {
		t.Error("Always = true — this asserts about an in-cluster reconciler, so on an " +
			"instance without one every lane fails forever. Opt-in is the whole point")
	}
}

// ASSERTIONS at `operating`, not invariants — the distinction the model draws and
// the one most easily lost here, because `reconcile-actions` holds invariants at
// the same state. An invariant is a property an extension MAINTAINS; these only
// observe. If a lane here ever starts repairing what it finds, that half is a
// transition and belongs in the runtime extension, not this one.
func TestLanesObserveRatherThanMaintain(t *testing.T) {
	for _, b := range Extension().Bindings {
		if b.Kind != extension.Assertion {
			t.Errorf("%s: kind = %s, want assertion — reconcile-actions MAINTAINS the lanes; "+
				"this extension only judges whether they worked", b.Name, b.Kind)
		}
		if b.State != extension.Operating {
			t.Errorf("%s: state = %s, want operating — this is drift-detection against a live "+
				"reconciler, not a one-shot check after a transition", b.Name, b.State)
		}
		for _, g := range b.Grants {
			if g != extension.ClusterRead {
				t.Errorf("%s holds %q — the assertion half of a capability/assertion pair must "+
					"stay read-only, or merging it with reconciler-runtime becomes indistinguishable "+
					"from leaving them separate", b.Name, g)
			}
		}
	}
}

// A reconciler can be healthy by the metrics-and-Lease measure while producing
// nothing in the cluster — which is exactly why the effects lane was written.
// Collapsing the two bindings would hide the failure the pair exists to separate.
func TestHealthAndEffectsStaySeparate(t *testing.T) {
	names := map[string]bool{}
	for _, b := range Extension().Bindings {
		names[b.Name] = true
	}
	for _, want := range []string{"functional-health", "effects"} {
		if !names[want] {
			t.Errorf("no binding named %q — a reconciler that is alive and leading but whose "+
				"lanes land nothing must be distinguishable from one that is simply down", want)
		}
	}
}
