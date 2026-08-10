package chartpublish

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cpWriteRepo writes rel→body files under a fresh temp root and returns the root.
func cpWriteRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestCPMax1 pins the retry-count floor: `--timeout / --interval` can round to 0
// (or go negative on a hand-set flag), and a 0 there would collapse the
// publish-and-wait loop to zero polls — a self-heal that never waits reports the
// chart still unpublished on a branch whose publish-charts run was mid-flight.
func TestCPMax1(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{-5, 1}, {0, 1}, {1, 1}, {2, 2}, {30, 30},
	} {
		if got := cpMax1(tc.in); got != tc.want {
			t.Errorf("cpMax1(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestCollectMissingPinsChecked pins the "checked" tally the success line reports.
// The whole point of that number is that a color.Green run states how many pins it
// actually verified — a miscount reintroduces the vacuous color.Green this command
// exists to prevent.
func TestCollectMissingPinsChecked(t *testing.T) {
	pins := []publishPin{
		{RepoURL: "ghcr.io/acme/charts", Chart: "llz-a", Version: "0.1.0"},
		{RepoURL: "ghcr.io/acme/charts", Chart: "llz-b", Version: "0.2.0"},
		{RepoURL: "ghcr.io/acme/charts", Chart: "llz-a", Version: "0.1.0"}, // duplicate → collapsed
		{RepoURL: "quay.io/acme/charts", Chart: "llz-c", Version: "0.3.0"}, // non-GHCR → out of scope
		{RepoURL: "ghcr.io", Chart: "llz-d", Version: "0.4.0"},             // unparseable → skipped
	}
	missing, checked, err := collectMissingPins(pins, func(_, repoPath, _ string) (bool, error) {
		return repoPath != "acme/charts/llz-b", nil
	})
	if err != nil {
		t.Fatalf("collectMissingPins: %v", err)
	}
	if checked != 2 {
		t.Errorf("checked = %d, want 2 (the two de-duplicated GHCR pins)", checked)
	}
	if len(missing) != 1 || missing[0].Chart != "llz-b" {
		t.Errorf("missing = %+v, want just llz-b", missing)
	}
}

// TestChartPublishCheckWaitLoopIsBounded pins the self-heal wait budget: exactly
// cpMax1(retries) polls, each preceded by one sleep, and a chart that only lands
// AFTER the budget still fails. An unbounded (or off-by-one) loop turns the
// preflight into an open-ended stall on a branch whose publish never happens.
func TestChartPublishCheckWaitLoopIsBounded(t *testing.T) {
	root := cpWriteRepo(t, map[string]string{
		"platform-apl/manifest/applications/cf.yaml": "spec:\n  source:\n    repoURL: ghcr.io/acme/charts\n" +
			"    chart: llz-cluster-foundation\n    targetRevision: 0.1.6\n",
	})

	sleeps, checks := 0, 0
	published := func(string, string, string) (bool, error) {
		checks++
		return checks > 5, nil // publishes only well past the 2-retry budget
	}
	out := captureStdout(t, func() {
		err := Run(Opts{
			Root: root, PublishIfMissing: true, Ref: "feat/x", TemplateRepo: "acme/lke-landing-zone",
			Retries: 2, Published: published,
			Dispatch: func(string, string, string) error { return nil },
			Sleep:    func(time.Duration) { sleeps++ },
		})
		if err == nil {
			t.Error("a chart that publishes after the wait budget must still fail the check")
		}
	})
	if sleeps != 2 {
		t.Errorf("wait loop slept %d time(s), want exactly the 2-retry budget", sleeps)
	}
	if !strings.Contains(out, "dispatching publish-charts.yml") {
		t.Errorf("self-heal must dispatch publish-charts.yml:\n%s", out)
	}
}

// A registry error raised DURING the wait must abort. Swallowing it leaves an
// empty "still missing" list, which reads as "everything published" — the exact
// vacuous color.Green this command was written to kill.
func TestChartPublishCheckWaitPropagatesRegistryError(t *testing.T) {
	root := cpWriteRepo(t, map[string]string{
		"platform-apl/manifest/applications/cf.yaml": "spec:\n  source:\n    repoURL: ghcr.io/acme/charts\n" +
			"    chart: llz-cluster-foundation\n    targetRevision: 0.1.6\n",
	})
	n := 0
	published := func(string, string, string) (bool, error) {
		n++
		if n == 1 {
			return false, nil // initial scan: unpublished → dispatch
		}
		return false, errors.New("ghcr boom")
	}
	var err error
	captureStdout(t, func() {
		err = Run(Opts{
			Root: root, PublishIfMissing: true, Ref: "feat/x", TemplateRepo: "acme/lke-landing-zone",
			Retries: 3, Published: published,
			Dispatch: func(string, string, string) error { return nil },
			Sleep:    func(time.Duration) {},
		})
	})
	if err == nil || !strings.Contains(err.Error(), "ghcr boom") {
		t.Errorf("registry error during the wait = %v, want it surfaced", err)
	}
}

// TestScanPublishPinsOnlyYAML pins the extension filter. A .md (or any non-YAML)
// file under a scanned tree is prose — parsing pin-shaped text out of it invents
// pins the registry will never have, and skipping real .yaml/.yml manifests
// hides the pins the gate exists to check.
func TestScanPublishPinsOnlyYAML(t *testing.T) {
	app := func(chart, version string) string {
		return "spec:\n  source:\n    repoURL: oci://ghcr.io/acme/charts\n" +
			"    chart: " + chart + "\n    targetRevision: " + version + "\n"
	}
	root := cpWriteRepo(t, map[string]string{
		"platform-apl/manifest/applications/foundation.yaml": app("llz-cluster-foundation", "0.1.10"),
		"platform-apl/components/openbao/openbao.yml":        app("llz-openbao-platform", "0.1.19"),
		// Documentation that quotes an Application manifest — must not be parsed.
		"platform-apl/README.md": "Example:\n\n" + app("llz-doc-example", "9.9.9"),
	})
	pins, err := scanPublishPins(root)
	if err != nil {
		t.Fatalf("scanPublishPins: %v", err)
	}
	got := map[string]string{}
	for _, p := range pins {
		got[p.Chart] = p.Version
	}
	if len(pins) != 2 || got["llz-cluster-foundation"] != "0.1.10" || got["llz-openbao-platform"] != "0.1.19" {
		t.Errorf("pins = %+v, want exactly the .yaml and .yml pins", got)
	}
	if _, ok := got["llz-doc-example"]; ok {
		t.Error("a chart pin was parsed out of a .md file")
	}
}

// TestExtractPublishPinsBlockScanEdges pins the two edges of the sibling scan:
// the reported line number (it lands in a ::error file=,line= annotation, so an
// off-by-one points a reviewer at the wrong line) and the bounds of the
// bidirectional block walk at both ends of the file.
func TestExtractPublishPinsBlockScanEdges(t *testing.T) {
	// repoURL sits on the very first line: the backward scan must examine index 0.
	pins := extractPublishPins("repoURL: ghcr.io/acme/charts\nchart: llz-first\ntargetRevision: 0.1.0\n")
	if len(pins) != 1 {
		t.Fatalf("pins = %+v, want 1 (repoURL on line 1 must be found)", pins)
	}
	if pins[0].RepoURL != "ghcr.io/acme/charts" || pins[0].Version != "0.1.0" {
		t.Errorf("pin = %+v, want the line-1 repoURL and the line-3 version", pins[0])
	}
	if pins[0].Line != 2 {
		t.Errorf("pin Line = %d, want 2 (1-based line of the chart: key)", pins[0].Line)
	}

	// A chart: line whose block runs to the end of the file must not walk past it.
	if got := extractPublishPins("chart: llz-orphan\n"); len(got) != 0 {
		t.Errorf("pins = %+v, want none (no repoURL/version siblings)", got)
	}
}
