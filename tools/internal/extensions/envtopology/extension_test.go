package envtopology

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("env-topology does not validate: %v", err)
		}
	}
}

// THE BINDING THAT IS NOT HERE is the point.
//
// branchpolicy.go PUTs a GitHub deployment-branch-policy — cloud-mutate at
// `configured`, which grantStates refuses. Rather than widen the row on ONE
// shipping case (the bar is two), the code went back to package main. If it ever
// returns here without the row being argued first, this fails.
func TestPackageDoesNotMutateExternalInfrastructure(t *testing.T) {
	for _, forbidden := range []string{
		// the branch-policy PUT and friends
		"deployment-branch-policies", "protection_rules",
	} {
		for _, f := range nonTestSources(t) {
			if containsCall(t, f, forbidden) {
				t.Errorf("%s reaches %s — that is cloud-mutate at `configured`, which the "+
					"ceiling refuses. Either argue the grantStates row with a SECOND shipping "+
					"case, or leave the mutation in cmd/llz where it is now", f, forbidden)
			}
		}
	}
}

// Two bindings, and they diverge on KIND rather than grants: env-set changes the
// spec, topology only reads it. Same grant, opposite side of the fence — the
// distinction a reader of `llz extension list` needs to see.
func TestEditAndReadStaySeparate(t *testing.T) {
	byName := map[string]extension.Binding{}
	for _, b := range Extension().Bindings {
		byName[b.Name] = b
		if b.State != extension.Configured {
			t.Errorf("%s: state = %s, want configured — topology IS part of what configured "+
				"means for an instance", b.Name, b.State)
		}
	}
	if byName["env-set"].Kind != extension.Transition {
		t.Error("env-set must be a transition — it edits the spec and re-renders, so a second " +
			"run after a change does not leave the repo as it found it")
	}
	if byName["topology"].Kind != extension.Assertion {
		t.Error("topology must be an assertion — reading which deployments exist changes nothing")
	}
}
