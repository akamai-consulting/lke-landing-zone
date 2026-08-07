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
	for _, b := range Extension().Bindings {
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

// The three cluster lanes fail independently and are wired into different CI
// lanes. Collapsing them into one binding would still validate — they hold
// identical grants — so the count is pinned here rather than left to Validate().
func TestThreeClusterLanesStayVisible(t *testing.T) {
	var verified int
	for _, b := range Extension().Bindings {
		if b.State == extension.Verified {
			verified++
		}
	}
	if verified != 3 {
		t.Errorf("want three assertion bindings at verified, got %d — a reader of `llz extension "+
			"list` should see three things that can go red, not one", verified)
	}
}
