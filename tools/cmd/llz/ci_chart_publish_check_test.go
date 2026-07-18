package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseOCIRef(t *testing.T) {
	cases := []struct {
		repoURL, chart, host, path string
		wantErr                    bool
	}{
		{"ghcr.io/acme/charts", "llz-foo", "ghcr.io", "acme/charts/llz-foo", false},
		{"oci://ghcr.io/acme/charts", "llz-bar", "ghcr.io", "acme/charts/llz-bar", false},
		{"https://ghcr.io/acme/charts/", "llz-baz", "ghcr.io", "acme/charts/llz-baz", false},
		{"ghcr.io", "llz-x", "", "", true}, // no path
		{"", "llz-x", "", "", true},        // empty
	}
	for _, c := range cases {
		host, path, err := parseOCIRef(c.repoURL, c.chart)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseOCIRef(%q,%q): want error, got %q/%q", c.repoURL, c.chart, host, path)
			}
			continue
		}
		if err != nil || host != c.host || path != c.path {
			t.Errorf("parseOCIRef(%q,%q) = %q,%q,%v; want %q,%q", c.repoURL, c.chart, host, path, err, c.host, c.path)
		}
	}
}

func TestExtractPublishPins(t *testing.T) {
	// Mirrors the apl-values Argo Application source shape (repoURL/chart/targetRevision
	// as same-indent siblings), plus a `version:` variant and a nested helm block that
	// must NOT be mistaken for the pin's siblings.
	const y = `
spec:
  source:
    repoURL: ghcr.io/acme/charts
    chart: llz-openbao-platform
    targetRevision: 0.1.13
    helm:
      releaseName: platform-openbao
      version: 9.9.9
---
spec:
  source:
    repoURL: ghcr.io/acme/charts
    chart: llz-cluster-foundation
    version: 0.1.6
`
	pins := extractPublishPins(y)
	got := map[string]string{}
	for _, p := range pins {
		got[p.Chart] = p.RepoURL + ":" + p.Version
	}
	if got["llz-openbao-platform"] != "ghcr.io/acme/charts:0.1.13" {
		t.Errorf("openbao pin = %q", got["llz-openbao-platform"])
	}
	if got["llz-cluster-foundation"] != "ghcr.io/acme/charts:0.1.6" {
		t.Errorf("cluster-foundation pin = %q", got["llz-cluster-foundation"])
	}
	// The nested helm.version (9.9.9) must not leak into the openbao pin.
	if got["llz-openbao-platform"] == "ghcr.io/acme/charts:9.9.9" {
		t.Error("nested helm.version leaked into the chart pin")
	}
}

func TestRunChartPublishCheck(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "apl-values", "components")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Two first-party pins + a placeholder pin (skipped) + a non-first-party pin (skipped).
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("published.yaml", "spec:\n  source:\n    repoURL: ghcr.io/acme/charts\n    chart: llz-cert-automation\n    targetRevision: 0.1.5\n")
	write("missing.yaml", "spec:\n  source:\n    repoURL: ghcr.io/acme/charts\n    chart: llz-cluster-foundation\n    targetRevision: 0.1.6\n")
	write("placeholder.yaml", "spec:\n  source:\n    repoURL: ghcr.io/<@ upstream_org @>/charts\n    chart: llz-openbao-platform\n    targetRevision: 0.1.13\n")
	write("upstream.yaml", "spec:\n  source:\n    repoURL: https://charts.external-secrets.io\n    chart: external-secrets\n    targetRevision: 0.10.7\n")

	// Fake registry: everything published EXCEPT llz-cluster-foundation:0.1.6.
	fake := func(host, repoPath, version string) (bool, error) {
		if repoPath == "acme/charts/llz-cluster-foundation" && version == "0.1.6" {
			return false, nil
		}
		return true, nil
	}
	// Preflight mode (no --publish-if-missing): the unpublished pin fails.
	if err := runChartPublishCheck(chartPublishOpts{root: root, published: fake}); err == nil {
		t.Fatal("want failure for the unpublished llz-cluster-foundation:0.1.6, got nil")
	}

	// With everything published, it passes.
	allOK := func(host, repoPath, version string) (bool, error) { return true, nil }
	if err := runChartPublishCheck(chartPublishOpts{root: root, published: allOK}); err != nil {
		t.Fatalf("want pass when all published, got %v", err)
	}
}

func TestRunChartPublishCheck_PublishIfMissing(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "apl-values")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cf.yaml"),
		[]byte("spec:\n  source:\n    repoURL: ghcr.io/acme/charts\n    chart: llz-cluster-foundation\n    targetRevision: 0.1.6\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The chart is missing until the dispatch "runs"; then it publishes on the next poll.
	dispatched := 0
	published := false
	dispatch := func(token, templateRepo, ref string) error {
		if templateRepo != "acme/lke-landing-zone" || ref != "feat/x" {
			t.Errorf("dispatch got repo=%q ref=%q", templateRepo, ref)
		}
		dispatched++
		return nil
	}
	check := func(host, repoPath, version string) (bool, error) { return published, nil }
	sleep := func(time.Duration) { published = true } // publish-charts "completes" on the first wait

	err := runChartPublishCheck(chartPublishOpts{
		root: root, publishIfMissing: true, ref: "feat/x", templateRepo: "acme/lke-landing-zone",
		retries: 5, published: check, dispatch: dispatch, sleep: sleep,
	})
	if err != nil {
		t.Fatalf("publish-if-missing should self-heal, got %v", err)
	}
	if dispatched != 1 {
		t.Errorf("expected exactly one publish-charts dispatch, got %d", dispatched)
	}

	// --publish-if-missing without --ref/--template-repo is a usage error.
	published = false
	if err := runChartPublishCheck(chartPublishOpts{
		root: root, publishIfMissing: true, retries: 1, published: func(string, string, string) (bool, error) { return false, nil },
		dispatch: dispatch, sleep: func(time.Duration) {},
	}); err == nil {
		t.Error("want error when --publish-if-missing lacks --ref/--template-repo")
	}
}

func TestGhcrShouldRetryAnon(t *testing.T) {
	cases := []struct {
		code      int
		haveCreds bool
		want      bool
	}{
		{401, true, true},   // creds rejected → retry anon (public charts)
		{403, true, true},   // the exact failure we hit
		{401, false, false}, // no creds sent → nothing to fall back from
		{403, false, false},
		{500, true, false}, // server error, not an auth problem
		{200, true, false}, // success, no retry
		{404, true, false},
	}
	for _, tc := range cases {
		if got := ghcrShouldRetryAnon(tc.code, tc.haveCreds); got != tc.want {
			t.Errorf("ghcrShouldRetryAnon(%d, %v) = %v, want %v", tc.code, tc.haveCreds, got, tc.want)
		}
	}
}

// TestScanPublishPinsCoversPlatformApl pins the scan-tree fix. The filter used to
// require "apl-values/" alone — a tree that holds no chart pin at all (an
// instance's is README.md + values.yaml) — so the check found zero pins and
// reported every chart published while verifying none, on every run including
// the release gate.
func TestScanPublishPinsCoversPlatformApl(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	app := func(chart, version string) string {
		return "apiVersion: argoproj.io/v1alpha1\nkind: Application\nspec:\n  source:\n" +
			"    repoURL: oci://ghcr.io/acme/charts\n    chart: " + chart + "\n    targetRevision: " + version + "\n"
	}
	// Where the pins actually live.
	write("platform-apl/manifest/applications/foundation.yaml", app("llz-cluster-foundation", "0.1.10"))
	write("platform-apl/components/openbao/openbao.yaml", app("llz-openbao-platform", "0.1.19"))
	// A tree the old filter DID match, holding no pins — the state that made the
	// check vacuous.
	write("apl-values/values.yaml", "# no chart pins here\n")

	pins, err := scanPublishPins(root)
	if err != nil {
		t.Fatalf("scanPublishPins: %v", err)
	}
	if len(pins) != 2 {
		t.Fatalf("found %d first-party pins, want 2 — platform-apl must be scanned: %+v", len(pins), pins)
	}
	got := map[string]string{}
	for _, p := range pins {
		got[p.Chart] = p.Version
	}
	if got["llz-cluster-foundation"] != "0.1.10" || got["llz-openbao-platform"] != "0.1.19" {
		t.Errorf("pins = %+v, want both platform-apl charts at their pinned versions", got)
	}
}

func TestUnderAny(t *testing.T) {
	for _, tt := range []struct {
		path string
		want bool
	}{
		{"platform-apl/manifest/applications/x.yaml", true},
		{"instance-template/apl-values/values.yaml", true},
		{"kubernetes-charts/llz-argo-bootstrap-apps/values.yaml", true},
		{"docs/designs/whatever.yaml", false},
		{"tools/cmd/llz/testdata/x.yaml", false},
	} {
		if got := underAny(tt.path, publishPinTrees); got != tt.want {
			t.Errorf("underAny(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}
