package chartguard

import (
	"strings"
	"testing"
)

// A path directly under kubernetes-charts/ belongs to no chart. Both shapes of
// "no chart component" must be dropped: no slash at all (a README), and a slash
// at offset 0 (a doubled separator), which would otherwise yield the charts root
// itself as a bogus "chart directory" and send the guard looking for
// kubernetes-charts/Chart.yaml.
func TestChangedChartDirsDropsPathsWithNoChartComponent(t *testing.T) {
	got := changedChartDirs([]string{
		chartsRoot + "README.md",    // no separator → no chart
		chartsRoot + "/orphan.yaml", // separator at offset 0 → still no chart
		chartsRoot + "llz-openbao/Chart.yaml",
		"docs/adr/0001.md", // outside the charts root entirely
	})
	want := []string{chartsRoot + "llz-openbao"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("changedChartDirs = %v, want %v", got, want)
	}
}
