package render

// THE RATCHET ON clusterspec.InertSpecFields, and it is the unusual kind: it
// asserts that something does NOT work.
//
// Each entry in that list is a spec field the repo accepts and renders nowhere.
// The list exists so `llz doctor` can tell an operator their setting reaches no
// cluster, instead of the silence that let `spec.alerting` go unrendered for the
// whole life of the managed platform. A list like that is only safe if it cannot
// go stale, and it goes stale in one direction: someone wires a field and leaves
// the row, so LLZ starts telling operators a working feature is broken.
//
// So: set every inert field to a marker no renderer could emit by chance, run the
// REAL render, and require the marker to be absent. **This test failing is the
// good outcome.** It means the field is wired and its row must be deleted in the
// same commit. The failure message says so, because a reader who meets a red test
// and reads it as "the render broke" will fix the wrong thing.
//
// It lives in render/ rather than clusterspec/ because renderTargets is what
// decides the answer, and asking clusterspec to predict its own renderer is the
// second-copy problem this whole area keeps producing.

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

func TestInertSpecFieldsAreStillInert(t *testing.T) {
	t.Setenv("LLZ_TEMPLATE_REF", "v0.0.0-golden")
	fields := clusterspec.InertSpecFields()
	if len(fields) == 0 {
		t.Skip("no inert fields declared — nothing to ratchet (this is the goal state)")
	}

	for _, f := range fields {
		t.Run(f.Path, func(t *testing.T) {
			lz, err := clusterspec.Decode([]byte(goldenSpec))
			if err != nil {
				t.Fatalf("decode goldenSpec: %v", err)
			}
			f.Marker(lz)

			const instRoot = "/inst"
			targets, err := renderTargets(lz, []string{"prod", "staging"},
				filepath.Join(instRoot, "terraform-iac-bootstrap"),
				filepath.Join(instRoot, "apl-values"), false)
			if err != nil {
				t.Fatalf("renderTargets: %v", err)
			}

			// PROVE THE MARKER COULD HAVE BEEN SEEN. Without this, a Marker that
			// silently failed to set anything (a renamed field, a value dropped by
			// a merge) would make the test pass for the wrong reason — it would be
			// hunting for a needle it never planted, forever.
			if !markerReachedTheSpec(lz) {
				t.Fatalf("%s: the marker did not survive into the spec, so this test would pass "+
					"whether or not the field renders. Fix the Marker func in clusterspec/inertfields.go",
					f.Path)
			}

			for path, body := range targets {
				if strings.Contains(body, inertMarkerNeedle) || strings.Contains(body, inertMarkerIntNeedle) {
					t.Errorf("%s NOW RENDERS — it reaches %s.\n\n"+
						"THIS IS GOOD NEWS AND THIS TEST IS THE CHORE THAT COMES WITH IT: delete "+
						"the matching row from clusterspec.InertSpecFields (inertfields.go) in this "+
						"same commit, and close out its entry in docs/upstream-asks.md. Leaving the "+
						"row would make `llz doctor` tell operators a working feature is broken.",
						f.Path, path)
				}
			}
		})
	}
}

// inertMarkerNeedle is the marker's stable prefix. Duplicated from clusterspec
// deliberately: this test is the CONSUMER of that constant, and importing it
// would let a rename silently change what is searched for on both sides at once —
// the exact way a split contract stops being one.
const inertMarkerNeedle = "LLZINERTPROBE9137"

// inertMarkerIntNeedle is the marker for the integer-typed fields, which cannot
// carry the string one — `components.observability.replicas` is an *int. Without
// searching for it too, that one field could be wired and this ratchet would not
// notice, which is the difference between a list that stays honest and one that
// covers three quarters of itself.
//
// "9137" alone is looser than the string needle, and deliberately: it is the
// shared suffix of both markers, so a reader grepping either finds both, and a
// rendered tree has no other reason to contain it. A false positive here costs a
// glance; a false negative costs the thing the list exists to prevent.
const inertMarkerIntNeedle = "9137"

// markerReachedTheSpec is the positive control: the marker is somewhere in the
// decoded spec after Marker ran.
func markerReachedTheSpec(lz *clusterspec.LandingZone) bool {
	for _, r := range lz.Spec.Alerting.Receivers {
		if strings.Contains(r, inertMarkerNeedle) {
			return true
		}
	}
	if strings.Contains(lz.Spec.Alerting.Slack.Channel, inertMarkerNeedle) ||
		strings.Contains(lz.Spec.Alerting.Slack.ChannelCrit, inertMarkerNeedle) {
		return true
	}
	for _, e := range lz.Spec.Environments {
		for _, t := range e.Components {
			if strings.Contains(t.Retention, inertMarkerNeedle) ||
				strings.Contains(t.Storage, inertMarkerNeedle) ||
				strings.Contains(t.RegistryStorage, inertMarkerNeedle) {
				return true
			}
			if t.Replicas != nil && strconv.Itoa(*t.Replicas) == inertMarkerIntNeedle {
				return true
			}
		}
	}
	return false
}

// AND THE FINDINGS MUST STAY QUIET ON AN INSTANCE THAT SETS NOTHING. A doctor
// warning every instance about fields it never touched is how a real finding gets
// tuned out.
func TestInertFindingsAreSilentWhenNothingIsSet(t *testing.T) {
	lz, err := clusterspec.Decode([]byte(goldenSpec))
	if err != nil {
		t.Fatal(err)
	}
	if got := clusterspec.InertFindings(lz); len(got) != 0 {
		t.Errorf("the golden spec sets no inert field, but doctor would report %d finding(s):\n%s",
			len(got), strings.Join(got, "\n"))
	}
}

// …AND MUST FIRE WHEN ONE IS. The negative control for the test above: without
// it, a Probe stuck at false would satisfy the silence test forever.
func TestInertFindingsFireForEveryDeclaredField(t *testing.T) {
	for _, f := range clusterspec.InertSpecFields() {
		t.Run(f.Path, func(t *testing.T) {
			lz, err := clusterspec.Decode([]byte(goldenSpec))
			if err != nil {
				t.Fatal(err)
			}
			f.Marker(lz)
			found := false
			for _, line := range clusterspec.InertFindings(lz) {
				if strings.Contains(line, f.Path) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s is declared inert, but setting it produces no doctor finding — "+
					"its Probe does not recognise what its Marker writes, so an operator who "+
					"configures it is told nothing", f.Path)
			}
		})
	}
}
