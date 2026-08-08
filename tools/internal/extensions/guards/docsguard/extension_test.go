package docsguard

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("guard-docs does not validate: %v", err)
		}
	}
}

// The declaration's honesty rests on this package holding NO write path: rendering
// is here, the os.WriteFile that `llz ci gen-toc` performs is in the command. If a
// write ever moves in, `gate:scaffolded[read-repo]` becomes a false statement and
// the validator will not catch it — the ceiling checks what a binding DECLARES,
// not what its code does.
//
// So assert it the only way a declaration can be held to its code: the package
// must not reach the filesystem for writing. os.ReadFile and os.Stat are the
// guard's whole job; os.WriteFile, os.Create, os.Remove and friends are not.
func TestPackageDeclaresNoWritePathBecauseItHasNone(t *testing.T) {
	for _, forbidden := range []string{
		"os.WriteFile", "os.Create", "os.Remove", "os.RemoveAll", "os.Rename", "os.Mkdir",
	} {
		for _, f := range nonTestSources(t) {
			if containsCall(t, f, forbidden) {
				t.Errorf("%s calls %s — this package declares gate:scaffolded[read-repo], "+
					"which says it only READS. Either move the write to the command (as gen-toc's is) "+
					"or change the declaration; a grant that is not true is worse than no grant.", f, forbidden)
			}
		}
	}
}

// The one grant a gate may hold. Widening it is a decision about where this code
// is allowed to run, not a detail.
func TestGuardDocsStaysFileInFindingsOut(t *testing.T) {
	e := Extension()
	if len(e.Bindings) != 1 {
		t.Fatalf("want exactly one binding, got %v", e.Bindings)
	}
	b := e.Bindings[0]
	if b.Kind != extension.Gate || b.State != extension.Scaffolded {
		t.Errorf("binding = %s, want gate:scaffolded", b)
	}
	if len(b.Grants) != 1 || b.Grants[0] != extension.ReadRepo {
		t.Errorf("grants = %v, want [read-repo] only", b.Grants)
	}
}
