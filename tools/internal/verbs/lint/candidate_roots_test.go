package lint

// candidate_roots_test.go — every Terraform root the instance layout knows about
// must be a root the linters actually visit.
//
// SPLIT CONTRACT. instancelayout.Roots is what the rest of llz treats as "the
// roots an instance carries"; candidateTFDirs is this package's own copy of the
// same idea, and the two drifted: `databases` was added to the layout and never
// to the lint list, so tflint, checkov and tf-validate silently skipped that root
// on every instance that declared a Managed Postgres. Nothing failed — a root can
// only be linted by a list it appears on, and an absent entry looks exactly like
// a root that does not exist.
//
// The two lists are deliberately NOT the same set, so this asserts containment
// rather than equality: `vpc` is linted here but is not in instancelayout.Roots
// (it is shared infrastructure with no per-env tfvars, which is what Roots
// enumerates). Containment is the direction that has bitten — a root the layout
// knows about escaping the linters.

import (
	"slices"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
)

func TestCandidateTFDirsCoverInstanceRoots(t *testing.T) {
	if len(instancelayout.Roots) == 0 {
		t.Fatal("instancelayout.Roots is empty — this gate would pass having compared nothing")
	}
	for _, root := range instancelayout.Roots {
		want := "terraform-iac-bootstrap/" + root
		if !slices.Contains(candidateTFDirs, want) {
			t.Errorf("instancelayout.Roots has %q but candidateTFDirs does not list %q — "+
				"tflint, checkov and tf-validate would all skip that root without saying so",
				root, want)
		}
	}
}
