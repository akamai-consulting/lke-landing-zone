package budget

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
)

// The declaration lives beside the code so the two cannot drift, which only helps
// if the declaration is checked. Validate here as well as in the registry: a
// package that stops being registered should not also stop being validated.
func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("guard-budgets does not validate: %v", err)
		}
	}
}

// The narrowness IS the reason the catalog picked this one to go first. Widening
// it is a decision, not a detail — a gate that reached a cluster or a credential
// would be doing so from the pre-commit path, against live infrastructure.
func TestGuardBudgetsStaysFileInFindingsOut(t *testing.T) {
	e := Extension()
	if len(e.Bindings) != 1 {
		t.Fatalf("want exactly one binding, got %v", e.Bindings)
	}
	b := e.Bindings[0]
	if b.Kind != extension.Gate || b.State != extension.Scaffolded {
		t.Errorf("binding = %s, want gate:scaffolded — these budgets are computable "+
			"from a checkout alone, with no environments resolved and no spec loaded", b)
	}
	if len(b.Grants) != 1 || b.Grants[0] != extension.ReadRepo {
		t.Errorf("grants = %v, want [read-repo] only", b.Grants)
	}
	if !e.Always {
		t.Error("guard-budgets must ship always-enabled: a ratchet an instance can " +
			"opt out of stops measuring the thing ADR 0014 exists to measure")
	}
}
