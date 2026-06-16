package main

// ci_chart_pin_guard.go implements `llz ci chart-pin-guard` — the companion to
// chart-version-guard. Where that guard asserts a changed chart bumps its
// Chart.yaml version (so publish-charts.yml actually pushes it), THIS guard
// asserts every place that PINS a first-party llz-* chart to a version pins the
// version that actually exists on disk (and therefore, post-publish, in the
// registry).
//
// Why it exists: a chart Chart.yaml version bump that is not mirrored into the
// Argo CD Application's `targetRevision` (live apl-values tree) or the
// llz-argo-bootstrap-apps component `version:` leaves Argo pulling a tag the
// registry never received. On a cold bootstrap that is silent and fatal: the
// pinned `llz-cluster-foundation:0.1.0` 404s, the support-plane app never syncs,
// the llz-openbao namespace is never created, and the OpenBao bootstrap workflow
// times out on `namespaces "llz-openbao" not found` with no hint at the cause.
// This guard turns that drift into a red PR instead of a dead cluster.
//
// The extraction + comparison logic is pure and unit-tested; the filesystem is
// reached only by the walk in RunE.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// chartPinScanRoots are the repo subtrees scanned for first-party chart pins:
// the live per-env Argo Application manifests and the app-of-apps generator's
// component list. Both pin chart versions that must track kubernetes-charts/.
var chartPinScanRoots = []string{"instance-template", "kubernetes-charts"}

// chartPinRe matches a `chart: <name>` line, capturing its indent and name.
// versionPinRe matches the sibling `targetRevision:`/`version:` line (the two
// keys Argo Applications and llz-argo-bootstrap-apps components respectively use
// for the pinned chart SemVer). Quotes are stripped by the caller.
var (
	chartPinRe   = regexp.MustCompile(`^(\s*)chart:\s*(\S+)\s*$`)
	versionPinRe = regexp.MustCompile(`^(\s*)(?:targetRevision|version):\s*(\S+)\s*$`)
)

// chartPin is a single first-party chart version pin found in a manifest.
type chartPin struct {
	Chart   string
	Version string
	Line    int // 1-based line number of the `chart:` line
}

// pinMismatch is a pin whose version disagrees with the on-disk Chart.yaml.
type pinMismatch struct {
	File  string
	Pin   chartPin
	WantV string // the local Chart.yaml version the pin should match
}

func ciChartPinGuardCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "chart-pin-guard",
		Short: "fail when an Argo chart pin drifts from the local Chart.yaml version",
		Long: "Scans the live apl-values Argo Application manifests and the\n" +
			"llz-argo-bootstrap-apps component list for first-party llz-* chart pins\n" +
			"(targetRevision / version) and fails if any pin disagrees with that chart's\n" +
			"kubernetes-charts/<chart>/Chart.yaml version. A pin ahead of (or behind) the\n" +
			"published chart 404s at Argo sync time — on a cold bootstrap that silently\n" +
			"strands the support-plane app and times out the OpenBao bootstrap.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runChartPinGuard(root)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root")
	return c
}

func runChartPinGuard(root string) error {
	local, err := loadLocalChartVersions(root)
	if err != nil {
		return fmt.Errorf("reading kubernetes-charts versions: %w", err)
	}

	byFile := map[string][]chartPin{}
	for _, sub := range chartPinScanRoots {
		base := filepath.Join(root, sub)
		if _, statErr := os.Stat(base); statErr != nil {
			continue // subtree absent in this layout — nothing to scan
		}
		walkErr := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// Helm chart templates/ hold Go-templated `chart: {{ ... }}`
				// values, not literal pins — skip them to avoid false matches.
				if d.Name() == "templates" {
					return fs.SkipDir
				}
				return nil
			}
			if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
				return nil
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if pins := extractChartPins(string(b)); len(pins) > 0 {
				rel, _ := filepath.Rel(root, path)
				byFile[filepath.ToSlash(rel)] = pins
			}
			return nil
		})
		if walkErr != nil {
			return fmt.Errorf("scanning %s: %w", sub, walkErr)
		}
	}

	checked, mismatches := countFirstPartyPins(byFile, local), checkChartPins(byFile, local)
	for _, m := range mismatches {
		fmt.Fprintf(os.Stderr,
			"::error file=%s,line=%d::%s is pinned to %s but kubernetes-charts/%s/Chart.yaml is %s — "+
				"update the pin to %s (a pin the registry never received 404s at Argo sync time)\n",
			m.File, m.Pin.Line, m.Pin.Chart, m.Pin.Version, m.Pin.Chart, m.WantV, m.WantV)
	}
	if len(mismatches) > 0 {
		return fmt.Errorf("chart-pin-guard: %d first-party chart pin(s) drifted from kubernetes-charts/", len(mismatches))
	}
	fmt.Printf("chart-pin-guard: %d first-party chart pin(s) match their Chart.yaml version.\n", checked)
	return nil
}

// extractChartPins returns every first-party chart version pin in a manifest.
// It pairs each `chart: <name>` line with the nearest following
// `targetRevision:`/`version:` line at the SAME indentation (its source-block
// sibling), stopping at the end of that block. Charts pinned by a `path:` (Argo
// git sources) carry no chart name here and are naturally skipped.
func extractChartPins(content string) []chartPin {
	lines := strings.Split(content, "\n")
	var pins []chartPin
	for i, line := range lines {
		m := chartPinRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		indent, name := m[1], strings.Trim(m[2], `"'`)
		for j := i + 1; j < len(lines); j++ {
			next := lines[j]
			if strings.TrimSpace(next) == "" {
				continue
			}
			curIndent := next[:len(next)-len(strings.TrimLeft(next, " \t"))]
			if len(curIndent) < len(indent) {
				break // dedented out of the source block before a version key
			}
			if len(curIndent) > len(indent) {
				continue // deeper nesting (e.g. helm.valuesObject) — not the sibling
			}
			if v := versionPinRe.FindStringSubmatch(next); v != nil {
				pins = append(pins, chartPin{
					Chart:   name,
					Version: strings.Trim(v[2], `"'`),
					Line:    i + 1,
				})
			}
			break // first same-indent sibling decides (version key or otherwise)
		}
	}
	return pins
}

// loadLocalChartVersions maps each first-party chart's name to its Chart.yaml
// version by reading kubernetes-charts/<dir>/Chart.yaml. Keyed on the chart's
// declared `name:` (the value pins reference), not the directory.
func loadLocalChartVersions(root string) (map[string]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "kubernetes-charts"))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw := readFileOrEmpty(filepath.Join(root, "kubernetes-charts", e.Name(), "Chart.yaml"))
		if raw == "" {
			continue
		}
		name, ver := chartName(raw), chartVersion(raw)
		if name != "" && ver != "" {
			out[name] = ver
		}
	}
	return out, nil
}

// chartName extracts the top-level `name:` value from Chart.yaml content, or ""
// when absent. Mirrors chartVersion (ci_chart_guard.go): only a column-0 key
// matches, so nested `name:` fields are not picked up.
func chartName(chartYAML string) string {
	for _, line := range strings.Split(chartYAML, "\n") {
		if strings.HasPrefix(line, "name:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "name:"))
		}
	}
	return ""
}

// countFirstPartyPins counts the pins across all files that reference a chart
// present in local — the denominator for the success message (third-party pins
// are not "checked" by this guard).
func countFirstPartyPins(byFile map[string][]chartPin, local map[string]string) int {
	n := 0
	for _, pins := range byFile {
		for _, pin := range pins {
			if _, ok := local[pin.Chart]; ok {
				n++
			}
		}
	}
	return n
}

// checkChartPins is the pure comparison core: given pins (per file) and the
// local chart→version map, it returns the mismatches. Pins for charts absent
// from local (upstream/third-party) are skipped. Sorted for stable output.
func checkChartPins(byFile map[string][]chartPin, local map[string]string) []pinMismatch {
	var out []pinMismatch
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		for _, pin := range byFile[f] {
			want, ok := local[pin.Chart]
			if !ok || pin.Version == want {
				continue
			}
			out = append(out, pinMismatch{File: f, Pin: pin, WantV: want})
		}
	}
	return out
}
