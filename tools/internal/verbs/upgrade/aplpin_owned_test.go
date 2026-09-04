package upgrade

// aplpin_owned_test.go — the pin sweep must not move a live platform.
//
// The sweep drops a pin that names a version llz itself set, on the stated grounds
// that "on managed App Platform the pin reaches no cluster anyway (Linode owns the
// deployed version)". spec.cluster.bootstrap.manageAplVersion inverts exactly that:
// the pin becomes what DEPLOYS. Dropping it moves the platform to this release's
// baseline and apl-core runs its runtime-upgrade migrations to get there — while
// the upgrade PR shows only a REMOVED LINE, with no version anywhere in the diff.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

func writeOwnedSpec(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The pin here names a PAST BASELINE, which is precisely what the sweep drops.
func ownedSpecs(t *testing.T, manage bool) string {
	t.Helper()
	root := t.TempDir()
	flag := ""
	if manage {
		flag = "\n        manageAplVersion: true"
	}
	writeOwnedSpec(t, root, clusterspec.LandingZoneFile,
		"spec:\n  defaults:\n    cluster:\n      bootstrap:"+flag+"\n")
	writeOwnedSpec(t, root, filepath.Join(clusterspec.EnvironmentsDir, "prod.yaml"),
		"spec:\n  environments:\n    prod:\n      cluster:\n        bootstrap:\n"+
			"          aplChartVersion: v6.2.0\n")
	return root
}

// THE POSITIVE CONTROL, first: without the opt-in the sweep still does its job.
// Without this, the refusal test below could pass because nothing was ever dropped.
func TestSweepStillDropsATrackingPinWhenTheVersionIsNotOwned(t *testing.T) {
	res, err := sweepAplPins(ownedSpecs(t, false), false)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Dropped) == 0 {
		t.Fatalf("premise: a past-baseline pin must be dropped when llz does not own the version (refused=%v kept=%v)",
			res.Refused, res.Kept)
	}
}

// ...and WITH the opt-in it must refuse, naming why.
func TestSweepRefusesToDropAPinThatDeploys(t *testing.T) {
	res, err := sweepAplPins(ownedSpecs(t, true), false)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Dropped) != 0 {
		t.Fatalf("manageAplVersion is set — the pin is what deploys and must NOT be dropped: %v", res.Dropped)
	}
	if len(res.Refused) == 0 {
		t.Fatal("the refusal must be REPORTED, not silent — an upgrade that quietly does nothing is its own defect")
	}
	if !strings.Contains(res.Refused[0].Reason, "manageAplVersion") {
		t.Errorf("the reason must name the field that changed the answer, got %q", res.Refused[0].Reason)
	}
}

// A ROOT-LEVEL opt-in protects the ENV pins too: mergeCluster falls an absent env
// value through to spec.defaults, so dropping any pin moves an opted-in deployment.
func TestSweepRefusesWhenOnlyTheRootOptsIn(t *testing.T) {
	root := t.TempDir()
	writeOwnedSpec(t, root, clusterspec.LandingZoneFile,
		"spec:\n  defaults:\n    cluster:\n      bootstrap:\n        manageAplVersion: true\n")
	writeOwnedSpec(t, root, filepath.Join(clusterspec.EnvironmentsDir, "prod.yaml"),
		"spec:\n  environments:\n    prod:\n      cluster:\n        bootstrap:\n          aplChartVersion: v6.2.0\n")
	res, err := sweepAplPins(root, false)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Dropped) != 0 {
		t.Errorf("a root opt-in reaches every env by inheritance: %v", res.Dropped)
	}
}

// AN UNREADABLE SPEC IS NOT EVIDENCE THAT NOBODY OPTED IN. The sweep runs over
// files that may not parse; treating a parse failure as "not owned" would drop pins
// on the strength of a file nobody could read.
func TestAnUnparseableSpecIsTreatedAsOwned(t *testing.T) {
	root := t.TempDir()
	writeOwnedSpec(t, root, clusterspec.LandingZoneFile, "spec:\n  defaults: [oops\n")
	writeOwnedSpec(t, root, filepath.Join(clusterspec.EnvironmentsDir, "prod.yaml"),
		"spec:\n  environments:\n    prod:\n      cluster:\n        bootstrap:\n          aplChartVersion: v6.2.0\n")
	res, err := sweepAplPins(root, false)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Dropped) != 0 {
		t.Errorf("an unreadable spec must not license a drop: %v", res.Dropped)
	}
}
