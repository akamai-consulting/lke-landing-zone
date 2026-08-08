package environments

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("environments does not validate: %v", err)
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

// bindings, keyed by name. Every assertion below reads from this rather than an
// index, because the merge that produced this extension reordered them once
// already and a positional test would have passed through the reorder.
func bindings(t *testing.T) map[string]extension.Binding {
	t.Helper()
	byName := map[string]extension.Binding{}
	for _, b := range Extension().Bindings {
		if b.Name == "" {
			t.Fatalf("every binding here must be named: four share two states, and Binding.Name " +
				"is what keeps them addressable")
		}
		byName[b.Name] = b
	}
	return byName
}

// The three `configured` bindings diverge on KIND rather than grants: add and set
// change the spec, topology only reads it. Same grant, opposite side of the fence
// — the distinction a reader of `llz extension list` needs to see.
func TestEditAndReadStaySeparate(t *testing.T) {
	byName := bindings(t)
	for _, n := range []string{"add", "set", "topology"} {
		if got := byName[n].State; got != extension.Configured {
			t.Errorf("%s: state = %s, want configured — topology IS part of what configured "+
				"means for an instance", n, got)
		}
	}
	for _, n := range []string{"add", "set"} {
		if byName[n].Kind != extension.Transition {
			t.Errorf("%s must be a transition — it edits the spec and re-renders, so a second "+
				"run after a change does not leave the repo as it found it", n)
		}
	}
	if byName["topology"].Kind != extension.Assertion {
		t.Error("topology must be an assertion — reading which deployments exist changes nothing")
	}
}

// THE MERGE'S LOAD-BEARING ASSERTION. `definition` is the only binding holding
// `write-repo`, and folding it into `add` would hand the topology READER
// permission to write landingzone.yaml. That is the over-granting argument
// `reconcile-actions` made when it split into four, and it is the whole reason
// three extensions became four named bindings rather than one.
func TestDefinitionKeepsItsWriteGrantToItself(t *testing.T) {
	byName := bindings(t)

	def := byName["definition"]
	if def.State != extension.Scaffolded {
		t.Errorf("definition: state = %s, want scaffolded — it writes landingzone.yaml before "+
			"there is anything configured to read", def.State)
	}
	if !hasGrant(def, extension.WriteRepo) {
		t.Error("definition must hold write-repo: it creates landingzone.yaml and writes " +
			"environments/<env>.yaml, which is the file-in-a-working-tree case the grant exists for")
	}
	for _, n := range []string{"add", "set", "topology"} {
		if hasGrant(byName[n], extension.WriteRepo) {
			t.Errorf("%s holds write-repo — it must not. The writes belong to `definition`; "+
				"widening these would make the union say every env verb can write the spec", n)
		}
	}
}

// No binding here reaches a cloud. The values are cloud FACTS (region, node type,
// object-storage cluster) but they arrive already resolved — config-readiness's
// `inputs-resolve` binding is what asks Linode, and it is separate precisely so
// this one can be a pure file writer with no network at all.
func TestNoCloudGrantAnywhere(t *testing.T) {
	for _, b := range Extension().Bindings {
		for _, g := range []extension.Grant{extension.CloudRead, extension.CloudMutate} {
			if hasGrant(b, g) {
				t.Errorf("%s holds %q — the resolvers moved to config-readiness so these verbs "+
					"could stay offline; a cloud grant here means that boundary slipped", b.Name, g)
			}
		}
	}
}

func hasGrant(b extension.Binding, want extension.Grant) bool {
	for _, g := range b.Grants {
		if g == want {
			return true
		}
	}
	return false
}
