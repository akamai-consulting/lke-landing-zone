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
	missing, checked, _, err := collectMissingPins(pins, func(_, repoPath, _ string) (bool, error) {
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
	// And it must have RIDDEN IT OUT first. Returning on the first registry error
	// killed the self-heal that had just been dispatched — and a GHCR secondary
	// rate limit is exactly what makes a publish need self-healing. The error is
	// still surfaced (above) because a budget that expired without a clean read
	// cannot conclude "still unpublished"; it must say which of the two happened.
	if n < 3 {
		t.Errorf("polled the registry %d time(s); one transient must not abort the wait — the retry "+
			"budget is what the self-heal runs on", n)
	}
	if !strings.Contains(err.Error(), "UNKNOWN") {
		t.Errorf("a budget that expired without a clean read must say the outcome is unknown, not report "+
			"the charts unpublished: %v", err)
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

// ── C09: the pins this check could not see ───────────────────────────────────

// TestExtractPublishPinsResolvesTheDefaultRegistry.
//
// `repoURL != "" && version != ""` dropped every pin that inherits
// `global.chartsRegistry` — which, measured on this repo, was all but ONE of
// them. llz-cluster-foundation and llz-cert-automation in
// kubernetes-charts/llz-argo-bootstrap-apps/values.yaml both omit repoURL, and
// those two are the exact charts this file's header cites as the reason the check
// exists. The len(pins)==0 vacuity guard could never fire either, because
// llz-openbao-platform DOES carry a repoURL and kept the count at one — a green
// run over a corpus of one, reported as "all pinned charts are published".
func TestExtractPublishPinsResolvesTheDefaultRegistry(t *testing.T) {
	const content = `
global:
  chartsRegistry: ghcr.io/acme/charts
components:
  clusterFoundation:
    source:
      type: oci
      # repoURL omitted -> defaults to global.chartsRegistry
      chart: llz-cluster-foundation
      version: 0.1.14
  openbao:
    source:
      type: oci
      repoURL: ghcr.io/acme/charts
      chart: llz-openbao-platform
      version: 0.1.22
  argoWorkflows:
    source:
      type: oci
      repoURL: https://argoproj.github.io/argo-helm
      chart: argo-workflows
      version: 0.45.0
`
	got := map[string]string{}
	for _, p := range extractPublishPins(content) {
		got[p.Chart] = p.RepoURL
	}
	if got["llz-cluster-foundation"] != "ghcr.io/acme/charts" {
		t.Errorf("a first-party pin with no repoURL must inherit global.chartsRegistry, got %q — "+
			"dropping it is why this check ran over one chart and called it all of them",
			got["llz-cluster-foundation"])
	}
	if got["llz-openbao-platform"] != "ghcr.io/acme/charts" {
		t.Errorf("an explicit repoURL must still win, got %q", got["llz-openbao-platform"])
	}
	// A THIRD-PARTY chart with no repoURL must NOT be resolved against our
	// registry: guessing would send the check looking for argo-workflows in GHCR.
	// (Here it has one, so it is only present to prove the llz- prefix is what
	// gates the fallback — see the sibling case below.)
	if got["argo-workflows"] != "https://argoproj.github.io/argo-helm" {
		t.Errorf("a third-party pin keeps its own repoURL, got %q", got["argo-workflows"])
	}
}

// TestTheDefaultRegistrySurvivesATrailingComment.
//
// The fallback regex was anchored `(\S+)\s*$`, so a trailing YAML comment on the
// `chartsRegistry:` line made it a non-match — the fallback went empty and every
// repoURL-less pin was dropped again, restoring the "ran over one chart and
// called it all of them" bug that this whole change exists to fix. Neither
// vacuity guard would catch it: the one pin carrying an explicit repoURL keeps
// len(pins) and checked at 1, so the run is green over a corpus of one. The
// values file already uses trailing comments on sibling keys, so this is a
// one-word edit away at all times.
func TestTheDefaultRegistrySurvivesATrailingComment(t *testing.T) {
	const content = `
global:
  chartsRegistry: ghcr.io/acme/charts # where first-party charts are published
components:
  clusterFoundation:
    source:
      type: oci
      chart: llz-cluster-foundation
      version: 0.1.14
`
	got := map[string]string{}
	for _, p := range extractPublishPins(content) {
		got[p.Chart] = p.RepoURL
	}
	if got["llz-cluster-foundation"] != "ghcr.io/acme/charts" {
		t.Errorf("a comment after chartsRegistry emptied the fallback: repoURL = %q, want ghcr.io/acme/charts", got["llz-cluster-foundation"])
	}
}

func TestExtractPublishPinsDoesNotAdoptThirdPartyCharts(t *testing.T) {
	const content = `
global:
  chartsRegistry: ghcr.io/acme/charts
components:
  x:
    source:
      chart: some-vendor-chart
      version: 1.2.3
`
	for _, p := range extractPublishPins(content) {
		if p.Chart == "some-vendor-chart" {
			t.Errorf("a third-party chart with no repoURL was resolved to %q — a missing repoURL there is a "+
				"template error, and guessing sends this check hunting for it in our registry", p.RepoURL)
		}
	}
}

// TestChartPublishCheckRefusesWhenAPinCouldNotBeRead. len(pins) > 0 says pins
// were PARSED; `checked` counts the ones resolved against GHCR. Every pin
// resolving to another host, or failing parseOCIRef, left checked at 0 and printed
// "0 pinned first-party chart(s) are published" — the same vacuous green the
// len(pins) guard exists to refuse, one step later.
//
// The two ways to reach checked == 0 want OPPOSITE answers, which is why they are
// separate arms here. A pin this code could not read is unverified for a reason
// nobody chose: hard failure. A pin on another registry is not applicable — this
// check only knows how to query GHCR — and failing a fork's e2e with "failed to
// parse as an OCI ref" points them at a parser they never touched.
func TestChartPublishCheckRefusesWhenAPinCouldNotBeRead(t *testing.T) {
	// An unreadable pin: repoURL with no path, so parseOCIRef cannot resolve it.
	root := cpWriteRepo(t, map[string]string{
		"platform-apl/manifest/applications/cf.yaml": "spec:\n  source:\n    repoURL: ghcr.io\n" +
			"    chart: llz-cluster-foundation\n    targetRevision: 0.1.6\n",
	})
	var err error
	captureStdout(t, func() {
		err = Run(Opts{Root: root, Published: func(string, string, string) (bool, error) { return true, nil }})
	})
	if err == nil || !strings.Contains(err.Error(), "resolved NONE") {
		t.Errorf("checking zero charts must not report them published; got %v", err)
	}
}

// TestChartPublishCheckSkipsAForkOnAnotherRegistry — the not-applicable arm. The
// skip must SAY what it did not verify; a silent nil would be the vacuous green
// again, just quieter.
func TestChartPublishCheckSkipsAForkOnAnotherRegistry(t *testing.T) {
	root := cpWriteRepo(t, map[string]string{
		"platform-apl/manifest/applications/cf.yaml": "spec:\n  source:\n    repoURL: registry.example.com/acme/charts\n" +
			"    chart: llz-cluster-foundation\n    targetRevision: 0.1.6\n",
	})
	var err error
	out := captureStdout(t, func() {
		err = Run(Opts{Root: root, Published: func(string, string, string) (bool, error) {
			t.Error("must not query GHCR for a chart pinned at another registry")
			return true, nil
		}})
	})
	if err != nil {
		t.Errorf("a fork publishing outside GHCR is not applicable, not broken: %v", err)
	}
	if !strings.Contains(out, "NOT checked here") {
		t.Errorf("the skip must name what went unverified, got:\n%s", out)
	}
}
