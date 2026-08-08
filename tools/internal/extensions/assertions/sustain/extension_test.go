package sustain

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("template-sustain does not validate: %v", err)
		}
	}
}

// THIS EXTENSION IS PARTIAL AND MUST SAY SO. It declares two bindings while the
// one that would hold `own-paths` is still in package main, welded to
// .template-manifest. An empty Incomplete here would make it read as complete —
// the ban-by-omission shape, one level up.
func TestItDeclaresWhatItCannotYetDeclare(t *testing.T) {
	e := Extension()
	if len(e.Incomplete) == 0 {
		t.Fatal("template-sustain is partial: the copier restore/overwrite pass has not moved, " +
			"and an extension that under-declares its own surface cannot be told from a complete one")
	}
	var mentionsOwnPaths bool
	for _, note := range e.Incomplete {
		if strings.Contains(note, string(extension.OwnPaths)) {
			mentionsOwnPaths = true
		}
	}
	if !mentionsOwnPaths {
		t.Error("the missing binding is the own-paths holder; the note should name it, " +
			"because that is the grant a reader will go looking for and not find")
	}
}

// `upgraded` is a recurring state and the first binding to attach to one other
// than `destroyed`. Assert both bindings stand alone, so a narrowing of
// bindableStates fails beside the code rather than in the model's own tests.
func TestBothBindingsAreIndividuallyExpressible(t *testing.T) {
	for _, b := range Extension().Bindings {
		one := extension.Extension{Name: "probe", Short: "x", Bindings: []extension.Binding{b}}
		if errs := one.Validate(); len(errs) > 0 {
			t.Errorf("%s cannot be declared on its own: %v", b, errs)
		}
	}
}

// Everything here reads. Sustain answers "where did this instance come from and
// how far behind is it" — repo questions. A mutating grant appearing would mean
// the restore pass had moved in without its ownership question being settled.
func TestSustainOnlyReads(t *testing.T) {
	for _, b := range Extension().Bindings {
		for _, g := range b.Grants {
			if g != extension.ReadRepo {
				t.Errorf("%s holds %q — template-sustain reads; the writing half is the "+
					"own-paths binding that has not moved", b, g)
			}
		}
	}
}
