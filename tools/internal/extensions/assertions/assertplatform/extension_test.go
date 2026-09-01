package assertplatform

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("assert-platform does not validate: %v", err)
		}
	}
}

// The first purely-assertion extension, so pin the property that makes it one: no
// binding here may hold a mutating grant. Fourteen extractions in, the failure
// mode this guards is specific — an assertion lane that starts "fixing" what it
// found, which is how `assert-storage` and `wedge-gameday` became the catalog's
// two flagged anomalies. If a lane here needs to mutate, the mutating half is a
// TRANSITION binding, not a widened assertion.
func TestEveryLaneOnlyObserves(t *testing.T) {
	mutating := map[extension.Grant]bool{
		extension.ClusterWrite:  true,
		extension.CloudMutate:   true,
		extension.SecretCustody: true,
		extension.OwnPaths:      true,
	}
	// THE EXCEPTION IS THE ONE THIS TEST ASKED FOR. It used to require that EVERY
	// binding be an assertion, while its own failure message said "declare the
	// mutating half as its own transition binding" — and when the capability layer
	// revealed that two lanes here really do mutate, following that instruction
	// broke the assertion above it. The rule is unchanged; what is pinned now is
	// that the mutation lives in exactly one named transition and nowhere else.
	var transitions int
	for _, b := range Extension().Bindings {
		if b.Kind == extension.Transition {
			transitions++
			if b.Name != "nudge-and-reap" {
				t.Errorf("unexpected transition %q — this extension observes a platform "+
					"someone else built; the one mutating lane is nudge-and-reap", b.Name)
			}
			continue
		}
		if b.Kind != extension.Assertion {
			t.Errorf("%s: kind = %s, want assertion — this extension observes a platform "+
				"someone else built", b.Name, b.Kind)
		}
		for _, g := range b.Grants {
			if mutating[g] {
				t.Errorf("%s holds %q — an assertion that changes what it measures is not an "+
					"assertion. Declare the mutating half as its own transition binding", b.Name, g)
			}
		}
	}
	if transitions != 1 {
		t.Errorf("%d transition bindings, want exactly 1 — every mutation here goes through "+
			"nudge-and-reap, and a second one is a new claim about what this extension does",
			transitions)
	}
}

// apl-version is the odd lane and the reason the set is split: it reads the
// PINNED CHART VERSION out of the spec file, before any cluster exists, so a
// 45-minute bootstrap cannot end in "that chart was never supported". Pin its
// state — drifting it to `verified` would quietly turn a preflight into a
// post-mortem.
func TestAplVersionIsAPreflightNotAPostMortem(t *testing.T) {
	var found bool
	for _, b := range Extension().Bindings {
		if b.Name != "apl-version" {
			continue
		}
		found = true
		if b.State != extension.Configured {
			t.Errorf("state = %s, want configured — it reads the spec's chart pin and needs no "+
				"cluster; running it at `verified` means discovering an unsupported chart AFTER "+
				"the bootstrap it would have prevented", b.State)
		}
		for _, g := range b.Grants {
			if g == extension.ClusterRead {
				t.Error("apl-version gained cluster-read — it reads a spec file. A cluster read " +
					"here would make the preflight unrunnable before there is a cluster, which " +
					"is the only moment it is useful")
			}
		}
	}
	if !found {
		t.Fatal("no binding named \"apl-version\"")
	}
}

// The cluster lanes fail independently and are wired into different CI lanes.
// Collapsing them into one binding would still validate — they hold identical
// grants — so the SET is pinned here rather than left to Validate().
//
// NAMED, NOT COUNTED. It used to assert `verified == 3`, which a fourth lane
// (argo-comparisons) failed with a message telling the reader to expect three —
// an assertion about growth wearing the words of an assertion about collapse.
// Listing the names keeps the property that was meant (each lane stays its own
// visible thing) and lets a genuinely new one arrive by being named.
func TestEveryClusterLaneStaysItsOwnVisibleBinding(t *testing.T) {
	want := map[string]bool{
		"health-workflow":  false,
		"argo-app":         false,
		"argo-comparisons": false,
		"instance-custom":  false,
	}
	var verified int
	for _, b := range Extension().Bindings {
		if b.State != extension.Verified {
			continue
		}
		verified++
		if _, known := want[b.Name]; !known {
			t.Errorf("unnamed verified binding %q — add it here with a word about what it can go "+
				"red for, or fold it into an existing lane deliberately", b.Name)
			continue
		}
		want[b.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("verified binding %q is gone — if two lanes were collapsed, a reader of "+
				"`llz extension list` lost one of the things that can go red", name)
		}
	}
	if verified != len(want) {
		t.Errorf("verified bindings = %d, want %d", verified, len(want))
	}
}
