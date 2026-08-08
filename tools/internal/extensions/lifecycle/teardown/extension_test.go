package teardown

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("teardown does not validate: %v", err)
		}
	}
}

// THE FIRST TRANSITION. Five extensions declared nothing but gates, assertions and
// invariants — things that observe or hold. This is the first that moves the
// platform, and `destroyed` is the state it moves it to. If this ever stops being
// a transition the model is back to describing only steady states.
func TestTheDestroyIsATransitionToDestroyed(t *testing.T) {
	var transitions int
	for _, b := range Extension().Bindings {
		if b.Kind != extension.Transition {
			continue
		}
		transitions++
		if b.State != extension.Destroyed {
			t.Errorf("%s: a destroy moves the platform to %q", b, extension.Destroyed)
		}
	}
	if transitions != 1 {
		t.Fatalf("want exactly one transition, got %d", transitions)
	}
}

// THE GATE MUST NOT BE ABLE TO MAKE ITS OWN VERDICT TRUE.
//
// assert-no-orphans is the destroy job's FINAL check: it re-counts what the
// destroy was supposed to remove. An assertion that could also delete would be
// able to clean up whatever it found and then report zero — and this is the case
// where that shortcut is most tempting, because the reaper is one call away.
func TestTheOrphanGateHoldsNoMutatingGrant(t *testing.T) {
	var assertions int
	for _, b := range Extension().Bindings {
		if b.Kind != extension.Assertion {
			continue
		}
		assertions++
		for _, g := range b.Grants {
			switch g {
			case extension.CloudMutate, extension.ClusterWrite, extension.SecretCustody:
				t.Errorf("assert-no-orphans holds %q — a gate that can delete what it counts "+
					"can make its own verdict true; the reaping is the transition's job", g)
			}
		}
	}
	if assertions != 1 {
		t.Fatalf("want exactly one assertion, got %d", assertions)
	}
}

// `destroyed` is a RECURRING state, and both kinds attaching to it is the model's
// own motivating example for letting assertions target any state rather than only
// `verified`. Assert each binding stands alone, so a narrowing of bindableStates
// fails here — next to the code — rather than in the model's own tests.
func TestBothBindingsAreIndividuallyExpressible(t *testing.T) {
	for _, b := range Extension().Bindings {
		one := extension.Extension{Name: "probe", Short: "x", Bindings: []extension.Binding{b}}
		if errs := one.Validate(); len(errs) > 0 {
			t.Errorf("%s cannot be declared on its own: %v", b, errs)
		}
	}
}
