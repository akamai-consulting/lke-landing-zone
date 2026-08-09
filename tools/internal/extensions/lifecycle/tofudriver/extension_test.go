package tofudriver

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("tofu-driver does not validate: %v", err)
		}
	}
}

// THE SPLIT IS THE POINT. The catalog files all three verbs under `→ provisioned`
// with cloud-mutate, and two of them mutate nothing.
//
// This matters more than the usual over-granting argument: `plan` and `output` are
// the two verbs a reviewer is MOST likely to assume are safe, and a grant line
// saying otherwise would either be ignored or would teach them to ignore grant
// lines. If plan ever starts applying, that is not a grant change — it is a bug.
func TestEachVerbDeclaresWhatItDoes(t *testing.T) {
	for _, b := range Extension().Bindings {
		mutates := false
		for _, g := range b.Grants {
			if g == extension.CloudMutate || g == extension.ClusterWrite || g == extension.SecretCustody {
				mutates = true
			}
		}
		switch b.Name {
		case "plan", "output":
			if mutates {
				t.Errorf("%s holds a mutating grant %v — a plan that applies is not a plan, and "+
					"reading an output changes nothing", b.Name, b.Grants)
			}
			if b.Kind != extension.Assertion || b.State != extension.Provisioned {
				t.Errorf("%s: binding = %s, want assertion:provisioned", b.Name, b)
			}
		case "apply", "import":
			// THE NAME OF THIS TEST (now TestEachVerbDeclaresWhatItDoes) USED TO BE
			// TestOnlyDestroyMutates, and it was true
			// while apply/import sat in package main. It is not any more: an apply is
			// the transition that REACHES `provisioned`, and declaring it read-only
			// would be the lie the `plan` case above exists to prevent.
			if !mutates {
				t.Errorf("%s dropped its mutating grant — apply creates the substrate, and "+
					"import writes Terraform state to a remote object-storage backend", b.Name)
			}
			if b.Kind != extension.Transition || b.State != extension.Provisioned {
				t.Errorf("%s: binding = %s, want transition:provisioned", b.Name, b)
			}
		case "destroy":
			if !mutates {
				t.Error("destroy dropped cloud-mutate — it still runs `tofu destroy`, so the " +
					"declaration would be the thing that is wrong")
			}
			if b.State != extension.Destroyed {
				t.Errorf("destroy state = %s, want destroyed — it is not a step toward having "+
					"infrastructure, it is the step that ends it", b.State)
			}
		default:
			t.Errorf("unexpected binding %q", b.Name)
		}
	}
}
