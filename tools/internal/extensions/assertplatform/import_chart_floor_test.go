package assertplatform

// import_chart_floor_test.go — an imported instance must be born on a SUPPORTED
// apl-core chart.
//
// THIS ASSERTION SPANS TWO EXTRACTIONS, and has now moved twice. It began inside
// the import tests, comparing the value a fixture passed in against the constant
// that fixture read — a copy against itself. It then moved to package main, whose
// comment said that was the side owning both halves. That stopped being true when
// `assert-apl-version` was extracted: the FLOOR lives here now, and the scaffold
// baseline is a clusterspec constant reachable from anywhere.
//
// The drift it guards is real and happened: the scaffolded version fell a major
// behind the baseline, and assert-apl-version then refused every instance import
// created.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

func TestImportScaffoldsASupportedChart(t *testing.T) {
	if clusterspec.AplSemverLess(clusterspec.BaselineAplChartVersion, MinSupportedAplChartVersion) {
		t.Errorf("llz import init scaffolds apl-core %s, below the supported floor %s — "+
			"`llz ci assert-apl-version` would refuse the instance it just created",
			clusterspec.BaselineAplChartVersion, MinSupportedAplChartVersion)
	}
}
