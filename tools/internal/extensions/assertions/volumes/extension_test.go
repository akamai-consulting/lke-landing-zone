package volumes

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("assert-storage does not validate: %v", err)
		}
	}
}

// THE RE-MODELLING IS THE POINT, so pin what it bought. The catalog flagged this
// entry as "holds cloud-mutate — the odd one out"; the model's answer was that a
// mutating half is a separate binding, not an exception. If someone folds them back
// together the declaration will be refused, but only if the assertion is still
// read-only — which is what this asserts directly.
func TestTheAssertionNeverHoldsAMutatingGrant(t *testing.T) {
	e := Extension()
	var assertions int
	for _, b := range e.Bindings {
		if b.Kind != extension.Assertion {
			continue
		}
		assertions++
		for _, g := range b.Grants {
			switch g {
			case extension.CloudMutate, extension.ClusterWrite, extension.SecretCustody:
				t.Errorf("the assertion holds %q — an assertion that mutates what it measures "+
					"cannot be trusted about what it found; declare the mutating half as its own binding", g)
			}
		}
	}
	if assertions != 1 {
		t.Fatalf("want exactly one assertion binding, got %d", assertions)
	}
	// And the union DOES include cloud-mutate, from the invariants. That the terse
	// listing and any single binding disagree is correct and is why --verbose exists.
	if !e.HasGrant(extension.CloudMutate) {
		t.Error("the extension as a whole must still declare cloud-mutate — the reconciler lanes do mutate")
	}
}

// The first declaration in the repo that needs Binding.Name: `operating` is the
// only state an invariant may attach to, so two invariants on one extension are
// indistinguishable without it and the duplicate-binding check rejects the pair.
func TestTheTwoInvariantsAreNamedAndDistinct(t *testing.T) {
	e := Extension()
	seen := map[string]bool{}
	var invariants int
	for _, b := range e.Bindings {
		if b.Kind != extension.Invariant {
			continue
		}
		invariants++
		if b.Name == "" {
			t.Errorf("invariant at %q has no name; two unnamed invariants collide", b.State)
		}
		if seen[b.Name] {
			t.Errorf("duplicate invariant name %q", b.Name)
		}
		seen[b.Name] = true
	}
	if invariants != 2 {
		t.Fatalf("want two invariants (volume-tags, volume-labels), got %d", invariants)
	}

	// Prove the names are load-bearing rather than decorative: strip them and the
	// declaration must be refused. Without this, dropping Binding.Name entirely
	// would leave every test above still passing.
	e = Extension()
	for i := range e.Bindings {
		e.Bindings[i].Name = ""
	}
	if errs := e.Validate(); len(errs) == 0 {
		t.Error("two unnamed invariants at the same state must be refused as duplicates")
	}
}

// The regression that produced the grantStates change. These two lanes are wired
// into reconcile.go and mutate Linode Volumes continuously, so `operating` has to
// be a legal home for cloud-mutate. If the row is ever narrowed back, this fails
// here — next to the code that needs it — rather than in a table nobody reads.
func TestTheReconcilerLanesCanDeclareWhatTheyDo(t *testing.T) {
	for _, b := range Extension().Bindings {
		if b.Kind != extension.Invariant {
			continue
		}
		one := extension.Extension{Name: "probe", Short: "x", Bindings: []extension.Binding{b}}
		if errs := one.Validate(); len(errs) > 0 {
			t.Errorf("%s cannot be declared: %v — this lane ships and mutates cloud resources "+
				"continuously; a ceiling that refuses it does not prevent it, it only stops it "+
				"being written down", b, errs)
		}
	}
}
