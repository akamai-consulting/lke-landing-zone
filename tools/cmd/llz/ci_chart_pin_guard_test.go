package main

import (
	"reflect"
	"testing"
)

func TestExtractChartPins(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []chartPin
	}{
		{
			name: "argo application targetRevision",
			content: "spec:\n" +
				"  source:\n" +
				"    repoURL: ghcr.io/org/charts\n" +
				"    chart: llz-cluster-foundation\n" +
				"    targetRevision: 0.1.1\n",
			want: []chartPin{{Chart: "llz-cluster-foundation", Version: "0.1.1", Line: 4}},
		},
		{
			name: "bootstrap-apps component version (chart before version, deeper valuesObject ignored)",
			content: "    source:\n" +
				"      type: oci\n" +
				"      chart: llz-openbao-platform\n" +
				"      version: 0.1.3\n" +
				"      valuesObject:\n" +
				"        chart: not-a-pin\n",
			want: []chartPin{{Chart: "llz-openbao-platform", Version: "0.1.3", Line: 3}},
		},
		{
			name: "quoted version is unquoted",
			content: "    chart: \"llz-cert-automation\"\n" +
				"    targetRevision: \"0.2.0\"\n",
			want: []chartPin{{Chart: "llz-cert-automation", Version: "0.2.0", Line: 1}},
		},
		{
			name: "git source (path, no version sibling) yields no pin",
			content: "    source:\n" +
				"      chart: llz-openbao\n" +
				"      repoURL: https://github.com/org/repo\n" +
				"      path: manifests/openbao\n",
			want: nil,
		},
		{
			name: "blank line between chart and version is tolerated",
			content: "    chart: llz-cert-automation\n" +
				"\n" +
				"    targetRevision: 0.1.0\n",
			want: []chartPin{{Chart: "llz-cert-automation", Version: "0.1.0", Line: 1}},
		},
		{
			name: "dedent before version key yields no pin",
			content: "  source:\n" +
				"    chart: llz-cluster-foundation\n" +
				"  destination:\n" +
				"    targetRevision: 9.9.9\n",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractChartPins(tt.content)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("extractChartPins() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestCheckChartPins(t *testing.T) {
	local := map[string]string{
		"llz-cluster-foundation": "0.1.1",
		"llz-openbao-platform":   "0.1.3",
	}
	byFile := map[string][]chartPin{
		"a.yaml": {
			{Chart: "llz-cluster-foundation", Version: "0.1.0", Line: 4}, // drifted
			{Chart: "external-secrets", Version: "0.10.7", Line: 9},      // upstream — skipped
		},
		"b.yaml": {
			{Chart: "llz-openbao-platform", Version: "0.1.3", Line: 3}, // matches
		},
	}
	got := checkChartPins(byFile, local)
	want := []pinMismatch{
		{File: "a.yaml", Pin: chartPin{Chart: "llz-cluster-foundation", Version: "0.1.0", Line: 4}, WantV: "0.1.1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("checkChartPins() = %#v, want %#v", got, want)
	}
}

func TestCheckChartPinsAllMatch(t *testing.T) {
	local := map[string]string{"llz-cert-automation": "0.1.0"}
	byFile := map[string][]chartPin{
		"x.yaml": {{Chart: "llz-cert-automation", Version: "0.1.0", Line: 2}},
	}
	if got := checkChartPins(byFile, local); len(got) != 0 {
		t.Errorf("checkChartPins() = %#v, want no mismatches", got)
	}
}

func TestChartName(t *testing.T) {
	yaml := "apiVersion: v2\nname: llz-openbao-platform\nversion: 0.1.3\n"
	if got := chartName(yaml); got != "llz-openbao-platform" {
		t.Errorf("chartName = %q, want llz-openbao-platform", got)
	}
	// A nested/indented name: must not be picked up as the chart name.
	if got := chartName("maintainers:\n  - name: someone\n"); got != "" {
		t.Errorf("chartName(nested only) = %q, want empty", got)
	}
}

func TestCountFirstPartyPins(t *testing.T) {
	local := map[string]string{"llz-cluster-foundation": "0.1.1"}
	byFile := map[string][]chartPin{
		"a.yaml": {
			{Chart: "llz-cluster-foundation", Version: "0.1.1"},
			{Chart: "argo-workflows", Version: "0.42.3"}, // upstream — not counted
		},
	}
	if got := countFirstPartyPins(byFile, local); got != 1 {
		t.Errorf("countFirstPartyPins = %d, want 1", got)
	}
}

// TestExtractChartPinsFindsVersionAboveChart pins the pairing fix. The scan used
// to run FORWARD only and break at the first same-indent sibling whatever it
// was, so a source block that writes the version key ABOVE `chart:` produced no
// pin — silently exempting that chart from drift checking. chart-publish-check
// reads the same YAML bidirectionally, so the two guards disagreed about which
// pins even exist.
func TestExtractChartPinsFindsVersionAboveChart(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "version below chart (always worked)",
			yaml: "  source:\n    chart: llz-foo\n    targetRevision: 1.2.3\n",
		},
		{
			name: "version above chart (previously missed entirely)",
			yaml: "  source:\n    targetRevision: 1.2.3\n    chart: llz-foo\n",
		},
		{
			name: "repoURL between them, version above",
			yaml: "  source:\n    targetRevision: 1.2.3\n    repoURL: oci://example.test\n    chart: llz-foo\n",
		},
		{
			name: "`version:` key instead of targetRevision",
			yaml: "  component:\n    version: 1.2.3\n    chart: llz-foo\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pins := extractChartPins(tt.yaml)
			if len(pins) != 1 {
				t.Fatalf("extractChartPins = %+v, want exactly 1 pin", pins)
			}
			if pins[0].Chart != "llz-foo" || pins[0].Version != "1.2.3" {
				t.Errorf("pin = %+v, want llz-foo@1.2.3", pins[0])
			}
		})
	}
}

// TestChartPinScanRootsCoverPlatformApl guards the coverage gap directly:
// platform-apl holds 3 of the repo's 5 first-party pins, and its absence from
// the scan roots let them drift unwatched while the guard reported green.
func TestChartPinScanRootsCoverPlatformApl(t *testing.T) {
	for _, want := range []string{"platform-apl", "kubernetes-charts"} {
		found := false
		for _, r := range chartPinScanRoots {
			if r == want {
				found = true
			}
		}
		if !found {
			t.Errorf("chartPinScanRoots is missing %q — first-party pins there would be exempt from drift checking while the guard still reports success: %v", want, chartPinScanRoots)
		}
	}
}
