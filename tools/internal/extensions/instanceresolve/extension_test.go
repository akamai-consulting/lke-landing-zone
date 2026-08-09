package instanceresolve_test

// This package is now ONLY a declaration -- the four resolvers moved to
// internal/shared/instanceresolve when the in-degree sweep found six peers
// importing them. There was no extension_test.go before, because the package's
// 87% came entirely from the library's own tests; when they left, the floor was
// measuring a package that no longer had any code under it.
//
// So this is what should always have been here: the declaration checked as a
// declaration. It is the same test every other extension carries, and it matters
// more here than usual, because a package whose only content IS a declaration has
// nothing else that would fail if the declaration went wrong.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/instanceresolve"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	e := instanceresolve.Extension()
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("declaration does not validate: %v", errs)
	}
	if e.Name != "instance-resolve" {
		t.Errorf("identity drifted: %q", e.Name)
	}
	if len(e.Incomplete) != 0 {
		t.Errorf("an Incomplete note appeared (%v) — this package is a declaration and "+
			"nothing else, so a gap here is a gap in the declaration itself", e.Incomplete)
	}
}

// THE KIND IS THE PART THAT WAS ARGUED. A gate was tried first and Validate()
// refused it: a gate permits read-repo and NOTHING else, because it runs in the
// fast pre-commit path over files alone. These checks call the Linode API, so they
// cannot answer from files -- which is the point of them, since the hardcoded
// region list they replaced went stale. Cheap-and-offline is the rule, not
// harmlessness, and this pins that reading.
func TestItIsAnAssertionBecauseItNeedsTheNetwork(t *testing.T) {
	e := instanceresolve.Extension()
	if len(e.Bindings) != 1 {
		t.Fatalf("want exactly one binding, got %v", e.Bindings)
	}
	b := e.Bindings[0]
	if b.Kind != extension.Assertion || b.State != extension.Scaffolded {
		t.Errorf("binding is %s, want assertion:scaffolded", b)
	}

	asGate := extension.Extension{
		Name: "probe", Short: "x",
		Bindings: []extension.Binding{{Kind: extension.Gate, State: b.State, Grants: b.Grants}},
	}
	if errs := asGate.Validate(); len(errs) == 0 {
		t.Error("a gate may now hold cloud-read — the gate rule is what forced this to be " +
			"an assertion, and its whole content is that a gate is CHEAP AND OFFLINE")
	}
}
