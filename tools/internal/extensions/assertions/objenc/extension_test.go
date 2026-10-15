package objenc

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("obj-encryption does not validate: %v", err)
		}
	}
}

// THE GATE HOLDS NO CUSTODY. Linode manages the SSE-S3 keys, so there is no key in
// this system for the gate to hold — and a binding that grew secret-custody would
// be a grant with nothing behind it, which is exactly the over-granting the
// per-binding scoping exists to prevent.
func TestTheAssertionHoldsNoCustody(t *testing.T) {
	e := Extension()
	if len(e.Bindings) != 1 || e.Bindings[0].Kind != extension.Assertion || e.Bindings[0].State != extension.Verified {
		t.Fatalf("want exactly one assertion:verified binding, got %v", e.Bindings)
	}
	for _, g := range e.Bindings[0].Grants {
		if g == extension.SecretCustody {
			t.Errorf("%s holds secret-custody — the gate only HEADs objects with a read credential", e.Bindings[0])
		}
	}
}

// ALWAYS ON, AND FOLLOWS NO COMPONENT. Every deployment writes to Object Storage,
// so every deployment is subject to the question; tying it to a component would
// make encryption at rest something an instance has to remember to ask about.
func TestObjEncryptionIsAlwaysOn(t *testing.T) {
	e := Extension()
	if !e.Always {
		t.Error("obj-encryption must ship enabled — every deployment's buckets are subject to it")
	}
	if e.Component != "" {
		t.Errorf("obj-encryption follows component %q; it must apply to every deployment", e.Component)
	}
}
