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
// baseline moved to v6.1.0 and re-asserted at v6.2.0: the SUPPORT FLOOR is a
// separate idea from the version this release targets. Nothing in 6.1.0 or 6.2.0
// made the landing zone 6.1/6.2-only, so a 6.0.0 instance must still pass the
// preflight and merely warn about drift.
func TestMinSupportedAplChartVersionIsNotTheBaseline(t *testing.T) {
	for _, v := range []string{"6.0.0", "6.1.0"} {
		if err := clusterspec.AplVersionSupported(v, "prod"); err != nil {
			t.Errorf("%s must remain SUPPORTED (floor %s) — a baseline bump raises the target, not the floor: %v",
				v, clusterspec.MinSupportedAplChartVersion, err)
		}
		if clusterspec.AplChartDriftOf(v) != clusterspec.AplChartDriftMinor {
			t.Errorf("a %s pin against the %s baseline must be MINOR drift (a warning), not a block",
				v, clusterspec.BaselineAplChartVersion)
		}
	}
}
