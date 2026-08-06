package chartguard

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("guard-charts does not validate: %v", err)
		}
	}
}

// A gate holds read-repo and nothing else. The version guard shells out to git,
// which is the one thing here that is not a file read — and it is still reading.
// If a mutating grant ever appears, one of these checks started fixing what it
// found, and a gate that edits the tree it is judging is not a gate.
func TestGuardChartsOnlyReads(t *testing.T) {
	e := Extension()
	if len(e.Bindings) != 1 {
		t.Fatalf("want one binding — three checks, one question, one moment; got %v", e.Bindings)
	}
	b := e.Bindings[0]
	if b.Kind != extension.Gate || b.State != extension.Scaffolded {
		t.Errorf("binding = %s, want gate:scaffolded", b)
	}
	if len(b.Grants) != 1 || b.Grants[0] != extension.ReadRepo {
		t.Errorf("grants = %v, want [read-repo] only", b.Grants)
	}
}
