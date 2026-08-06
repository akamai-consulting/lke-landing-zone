package promote

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("promote-pipeline does not validate: %v", err)
		}
	}
}

// THIS IS THE TEST THE FILE SPLIT EXISTS FOR.
//
// The declaration says `read-repo`. This extension's whole job is to produce a
// file — so the write was moved to cmd/llz to keep that grant true, and the
// validator cannot tell the difference: the ceiling checks what a binding
// DECLARES, not what its code does.
//
// Same check internal/docsguard runs for the same reason (`llz ci gen-toc`). If a
// third package needs it the helper is worth sharing; two is a copy.
func TestPackageContainsNoWritePath(t *testing.T) {
	for _, forbidden := range []string{
		"os.WriteFile", "os.Create", "os.Remove", "os.RemoveAll", "os.Rename", "os.Mkdir", "os.MkdirAll",
	} {
		for _, f := range nonTestSources(t) {
			if containsCall(t, f, forbidden) {
				t.Errorf("%s calls %s — this package declares transition:promoted[read-repo], "+
					"which says it only READS. PlanWorkflow returns the rendered content and "+
					"cmd/llz performs the write; keep it that way, or change the declaration. "+
					"A grant that is not true is worse than no grant.", f, forbidden)
			}
		}
	}
}

// `promoted` was the last unclaimed state in the vocabulary. Pin it: a state
// nothing binds is indistinguishable from one that should not exist, and this is
// the only extension that binds this one.
func TestPromotePipelineBindsPromoted(t *testing.T) {
	e := Extension()
	if len(e.Bindings) != 1 {
		t.Fatalf("want exactly one binding, got %v", e.Bindings)
	}
	b := e.Bindings[0]
	if b.Kind != extension.Transition {
		t.Errorf("kind = %s, want transition — a rank change produces a different workflow, so "+
			"a second run does not leave the repo as it found it", b.Kind)
	}
	if b.State != extension.Promoted {
		t.Errorf("state = %s, want promoted — this is the only extension binding it", b.State)
	}
	if len(b.Grants) != 1 || b.Grants[0] != extension.ReadRepo {
		t.Errorf("grants = %v, want [read-repo] only. own-paths is the tempting mistake: it is a "+
			"copier FENCE, not a write permit, and promote.yml is a copier-rendered merge stub", b.Grants)
	}
}
