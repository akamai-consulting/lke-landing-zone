package chartguard

// lockdiscovery_test.go — chart-lock-drift discovers its own corpus.
//
// THE DISCOVERY PATH IS WHERE A VACUOUS PASS WOULD ARRIVE UNNOTICED, which is why
// it is tested separately from the drift logic. When the caller names the charts, a
// typo shows up as "no Chart.yaml" against a path they typed. When nobody names
// them, a discovery that quietly found nothing reports the same clean as one that
// checked every chart — and there is no typed argument to look at afterwards.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirstPartyChartDirsFindsEveryChart(t *testing.T) {
	root := t.TempDir()
	for _, c := range []string{"llz-a", "llz-b"} {
		mkChart(t, root, c)
	}
	// A directory with no Chart.yaml is not a chart. Reporting it would produce a
	// finding about a directory rather than about a chart.
	if err := os.MkdirAll(filepath.Join(root, chartsDir, "not-a-chart"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A stray file alongside the chart dirs must not become a subject either.
	if err := os.WriteFile(filepath.Join(root, chartsDir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := firstPartyChartDirs(root)
	if err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	want := []string{
		filepath.Join(chartsDir, "llz-a"),
		filepath.Join(chartsDir, "llz-b"),
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("discovered %v, want %v — a chart missed here is a chart the guard "+
			"never checks, and nothing downstream would say so", got, want)
	}
}

// SORTED, because the guard prints one line per chart and output that reorders
// between runs produces a diff every run — the reason guardwalk sorts its findings.
func TestFirstPartyChartDirsIsOrdered(t *testing.T) {
	root := t.TempDir()
	for _, c := range []string{"llz-z", "llz-a", "llz-m"} {
		mkChart(t, root, c)
	}
	got, err := firstPartyChartDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("discovery is unordered: %v", got)
		}
	}
}

// AN EMPTY CORPUS IS A FAILURE, NOT A CLEAN RUN. This is the rule every corpus
// guard in this tree keeps (guardkit.RequireCorpus), applied to the set the guard
// chose for itself.
func TestFirstPartyChartDirsRefusesAnEmptyCorpus(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, chartsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := firstPartyChartDirs(root)
	if err == nil {
		t.Fatal("an empty kubernetes-charts/ returned no error — the guard would report " +
			"every lock in sync having examined nothing")
	}
	if !strings.Contains(err.Error(), "examined nothing") {
		t.Errorf("error %q does not say what went wrong", err)
	}
}

// A MISSING TREE IS ALSO A FAILURE, and a distinguishable one: "there are no
// charts" and "the chart tree moved" need different fixes.
func TestFirstPartyChartDirsRefusesAMissingTree(t *testing.T) {
	if _, err := firstPartyChartDirs(t.TempDir()); err == nil {
		t.Fatal("a missing kubernetes-charts/ returned no error")
	}
}

func mkChart(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, chartsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("name: "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
