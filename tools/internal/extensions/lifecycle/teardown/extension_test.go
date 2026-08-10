package teardown

import (
	"net/http"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
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

// A DRY RUN MUST NOT BE ABLE TO DELETE, ENFORCED RATHER THAN TRUSTED.
//
// Until now that property rested on one early `return` inside Deleter's closure —
// the only thing between `--dry-run` and a destroyed cluster. The binding
// selector puts a second, independent refusal at the transport, so this asserts
// the selector actually narrows rather than merely being called.
func TestTheDryRunBindingCannotDelete(t *testing.T) {
	read := cloudBinding(false)
	if err := capability.CloudFor(read).Permits(http.MethodDelete); err == nil {
		t.Error("the non-mutating binding permits DELETE — a dry run could destroy through it")
	}
	if err := capability.CloudFor(read).Permits(http.MethodGet); err != nil {
		t.Errorf("the non-mutating binding must still LIST, or a dry run cannot report: %v", err)
	}

	// And the mutating one must actually be able to, or `--yes` is broken.
	if err := capability.CloudFor(cloudBinding(true)).Permits(http.MethodDelete); err != nil {
		t.Errorf("the mutating binding cannot DELETE: %v", err)
	}
}

// SELECTION MAY ONLY NARROW, and this compares EFFECTIVE PERMISSION rather than
// the grant lists — which is the level the property actually lives at.
//
// The first version of this test compared the two Grants slices and failed: the
// assertion declares `cloud-read` and the transition declares `cloud-mutate`
// WITHOUT it, so read looked like a grant the wide binding lacked. It is not —
// cloud-mutate implies cloud-read, exactly as cluster-write implies cluster-read.
// Comparing the literal lists tested a proxy for the property and got the wrong
// answer about a correct declaration.
//
// The rule that keeps the model's guarantee intact now that a binding can be
// chosen at runtime: for EVERY method the narrow binding permits, the wide one
// must permit it too. The maximum a code path may do stays static and readable
// from the declaration; the flag subtracts from it and never adds. If that ever
// stopped holding, "narrowing" would be "swapping" and reading the declaration
// would stop bounding the behaviour.
func TestTheRuntimeSelectionOnlyEverNarrows(t *testing.T) {
	wide := capability.CloudFor(cloudBinding(true))
	narrow := capability.CloudFor(cloudBinding(false))

	read, mutate := capability.ClassifiedMethods()
	for _, m := range append(append([]string{}, read...), mutate...) {
		if narrow.Permits(m) == nil && wide.Permits(m) != nil {
			t.Errorf("the non-mutating binding permits %s and the mutating one does not — "+
				"selection must SUBTRACT permission, never swap it", m)
		}
	}

	// And it must genuinely subtract something, or the selector is decorative.
	var subtracted bool
	for _, m := range mutate {
		if wide.Permits(m) == nil && narrow.Permits(m) != nil {
			subtracted = true
		}
	}
	if !subtracted {
		t.Error("the two bindings permit the same methods — the runtime selection buys nothing, " +
			"and a dry run is back to being protected only by Deleter's early return")
	}
}
