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

// Both are partial, and both say so. The rotation table is the third wall.
func TestPartialsStayMarked(t *testing.T) {
	for _, e := range []extension.Extension{PATExtension(), ObjKeyExtension()} {
		inc := strings.ToLower(strings.Join(e.Incomplete, " "))
		if inc == "" {
			t.Fatalf("%s: Incomplete emptied — its mint/in-cluster paths are still in package main", e.Name)
		}
		if !strings.Contains(inc, "rotation table") {
			t.Errorf("%s: the note no longer names the wall that kept the rest back", e.Name)
		}
	}
}
