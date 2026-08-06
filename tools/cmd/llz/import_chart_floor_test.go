package main

// import_chart_floor_test.go — an imported instance must be born on a SUPPORTED
// apl-core chart.
//
// THIS ASSERTION SPANS THE EXTRACTION BOUNDARY, and moved to the side that owns
// both halves: defaultAplChartVersion is what brownfield.Deps supplies, and
// minSupportedAplChartVersion is what `llz ci assert-apl-version` enforces. Both
// are package main.
//
// It is also STRONGER here than it was inside the import tests. There it compared
// the value the fixture passed in against the constant the fixture read — a copy
// against itself. Here it compares the two independent constants, which is the
// drift that actually happened: the scaffolded version fell a major behind the
// baseline, and assert-apl-version then refused every instance import created.

import "testing"

func TestImportScaffoldsASupportedChart(t *testing.T) {
	if semverLess(defaultAplChartVersion, minSupportedAplChartVersion) {
		t.Errorf("llz import init scaffolds apl-core %s, below the supported floor %s — "+
			"`llz ci assert-apl-version` would refuse the instance it just created",
			defaultAplChartVersion, minSupportedAplChartVersion)
	}
}
