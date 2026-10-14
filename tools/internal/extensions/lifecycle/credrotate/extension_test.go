package credrotate

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestBothDeclarationsValidate(t *testing.T) {
	for _, e := range []extension.Extension{PATExtension(), ObjKeyExtension()} {
		if errs := e.Validate(); len(errs) != 0 {
			t.Fatalf("%s does not validate: %v", e.Name, errs)
		}
		if !e.Always {
			t.Errorf("%s: these are the credentials every instance holds", e.Name)
		}
	}
}

// TWO EXTENSIONS, ONE PACKAGE. They share a framework, not a lifecycle: an
// instance with no object storage has no reason to rotate an OBJ key, and one that
// never issues PATs still needs its buckets. Collapsing them into one extension
// would tie two independent enable/disable decisions to a single switch.
func TestTheTwoStaySeparate(t *testing.T) {
	if PATExtension().Name == ObjKeyExtension().Name {
		t.Fatal("the two rotators collapsed into one extension — they share a framework, " +
			"not a lifecycle, and an instance can legitimately want one without the other")
	}
	for _, e := range []extension.Extension{PATExtension(), ObjKeyExtension()} {
		if len(e.Bindings) != 1 {
			t.Errorf("%s: want one binding, got %d", e.Name, len(e.Bindings))
		}
	}
}

// Both MINT and PLACE credential material. Dropping either grant would describe a
// rotation that only reads.
func TestBothDeclareMintAndCustody(t *testing.T) {
	for _, e := range []extension.Extension{PATExtension(), ObjKeyExtension()} {
		for _, g := range []extension.Grant{extension.CloudMutate, extension.SecretCustody} {
			if !e.HasGrant(g) {
				t.Errorf("%s: %q dropped — it mints a credential and writes it to every "+
					"infra-<deployment> environment", e.Name, g)
			}
		}
	}
}

// BOTH ROWS ARE WHOLE NOW, so this runs the other way. It used to assert each
// note stayed non-empty and kept naming "the rotation table" — the wall that held
// the mint and in-cluster paths in package main. That wall came down and the paths
// followed: rotate-incluster-pat, mint-bootstrap-objkeys and temp-objkey are this
// package, rotate-broad-pat went to assertsecrets. A note reappearing means a path
// left again, and that should be argued rather than typed.
func TestNoPathsAreOutstanding(t *testing.T) {
	for _, e := range []extension.Extension{PATExtension(), ObjKeyExtension()} {
		if inc := strings.Join(e.Incomplete, " "); inc != "" {
			t.Errorf("%s: Incomplete came back (%q) — name the path that left and why", e.Name, inc)
		}
	}
}
