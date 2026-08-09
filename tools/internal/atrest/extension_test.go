package atrest

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("posture-at-rest does not validate: %v", err)
		}
	}
}

// The first non-gate binding in the registry, and the point of declaring it. If
// this ever reverts to a gate, the model is back to being exercised by one shape
// only — which is the state the third extension was chosen to leave.
func TestPostureAtRestIsAnInvariantNotAGate(t *testing.T) {
	e := Extension()
	if len(e.Bindings) != 1 {
		t.Fatalf("want exactly one binding, got %v", e.Bindings)
	}
	b := e.Bindings[0]
	if b.Kind != extension.Invariant {
		t.Errorf("kind = %q, want %q — encryption is decided at CREATE and is immutable, so the "+
			"claim is about the running system rather than about a change", b.Kind, extension.Invariant)
	}
	if b.State != extension.Operating {
		t.Errorf("state = %q, want %q (the only state an invariant may attach to)", b.State, extension.Operating)
	}
}

// `operating` is the ONLY state bindableStates allows an invariant, and this is
// the first declaration that depends on it. Assert the rule from the outside so
// the constraint is pinned by a real declaration rather than only by the model's
// own tests: an invariant moved anywhere else must be refused, with a reason.
func TestAnInvariantIsRefusedAnywhereButOperating(t *testing.T) {
	for _, s := range []extension.State{
		extension.Scaffolded, extension.Configured, extension.Provisioned,
		extension.Seeded, extension.Converged, extension.Verified, extension.Destroyed,
	} {
		e := Extension()
		e.Bindings[0].State = s
		if errs := e.Validate(); len(errs) == 0 {
			t.Errorf("an invariant at %q validated; only %q is legal", s, extension.Operating)
		}
	}
}
