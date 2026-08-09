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
// one that would hold `own-paths` is still in internal/cli, welded to
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
func TestEveryBindingIsIndividuallyExpressible(t *testing.T) {
	for _, b := range Extension().Bindings {
		one := extension.Extension{Name: "probe", Short: "x", Bindings: []extension.Binding{b}}
		if errs := one.Validate(); len(errs) > 0 {
			t.Errorf("%s cannot be declared on its own: %v", b, errs)
		}
	}
}

// THE ONE MUTATION IS THE LOCK REFRESH, AND NOTHING ELSE MAY MUTATE.
//
// This test used to assert that every grant here was read-repo, on the reasoning
// that sustain answers "where did this instance come from and how far behind is
// it" — repo questions. That was true of the declaration and false of the code:
// `llz ci managed-fresh --write` regenerates .template-managed.lock, and the
// extension had no binding covering it.
//
// So the property splits in two. `own-paths` must still be ABSENT — the copier
// restore pass has not moved in and its ownership question is unsettled, which is
// what the original test was really protecting and is still worth protecting.
// And write-repo is permitted on exactly one binding: the transition that does
// the writing.
func TestOnlyTheLockRefreshMayMutate(t *testing.T) {
	for _, b := range Extension().Bindings {
		for _, g := range b.Grants {
			switch {
			case g == extension.ReadRepo:
			case g == extension.WriteRepo && b.Name == "lock-refresh":
			case g == extension.OwnPaths:
				t.Errorf("%s holds own-paths — the copier restore pass has not moved here, and "+
					"its ownership question (ADR 0014) is the reason it has not", b)
			default:
				t.Errorf("%s holds %q — sustain reads, apart from the lock-refresh transition "+
					"that regenerates .template-managed.lock", b, g)
			}
		}
	}

	// And the writing binding must actually exist, or `--write` is back to
	// mutating under a declaration that does not cover it.
	var found bool
	for _, b := range Extension().Bindings {
		if b.Name == "lock-refresh" {
			found = true
			if b.Kind != extension.Transition {
				t.Errorf("lock-refresh is a %s — the validator refuses a mutating gate or "+
					"assertion and says to declare the mutating half as its own transition", b.Kind)
			}
		}
	}
	if !found {
		t.Error("no lock-refresh binding — `llz ci managed-fresh --write` writes the lock, so " +
			"removing it restores the false declaration this test was corrected for")
	}
}
