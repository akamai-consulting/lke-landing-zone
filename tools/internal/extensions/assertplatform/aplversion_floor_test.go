package assertplatform

// The SUPPORT FLOOR is this package's fact now, so its test came with it.
//
// It was in package main's ci_bootstrap_cluster_test.go because that is where
// defaultAplChartVersion lives — but the assertion it makes is about the floor
// being a SEPARATE idea from the baseline, and the floor moved here with the
// apl-version lane.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// TestMinSupportedAplChartVersionIsNotTheBaseline guards the split made when the
// baseline moved to v6.1.0: the SUPPORT FLOOR is a separate idea from the version
// this release targets. Nothing in 6.1.0 made the landing zone 6.1-only, so a
// 6.0.0 instance must still pass the preflight and merely warn about drift.
func TestMinSupportedAplChartVersionIsNotTheBaseline(t *testing.T) {
	if err := clusterspec.AplVersionSupported("6.0.0", "prod"); err != nil {
		t.Errorf("6.0.0 must remain SUPPORTED (floor %s) — the 6.1.0 bump raises the target, not the floor: %v",
			clusterspec.MinSupportedAplChartVersion, err)
	}
	if clusterspec.AplChartDriftOf("6.0.0") != clusterspec.AplChartDriftMinor {
		t.Error("a 6.0.0 pin against the v6.1.0 baseline must be MINOR drift (a warning), not a block")
	}
}
