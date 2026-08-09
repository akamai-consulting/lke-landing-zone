package charty

// The behaviour worth pinning is that these read files a YAML parser would
// REJECT. Chart.yaml and values.yaml in this repo are templates — copier
// placeholders and Helm expressions — so line-scanning is the point, not a
// shortcut, and a well-meaning rewrite to yaml.Unmarshal would break the guards
// that depend on it while still passing a naive test on well-formed input.

import "testing"

func TestChartNameAndVersion(t *testing.T) {
	const chart = `apiVersion: v2
name: platform-harbor
version: 1.4.2
description: something
`
	if got := ChartName(chart); got != "platform-harbor" {
		t.Errorf("ChartName = %q", got)
	}
	if got := ChartVersion(chart); got != "1.4.2" {
		t.Errorf("ChartVersion = %q", got)
	}
}

func TestScalarsReadThroughTemplating(t *testing.T) {
	// Not valid YAML: a bare {{ }} scalar is a parse error. These files are
	// rendered later, and the guards must read them before that happens.
	const templated = `apiVersion: v2
name: {{ chart_name }}
version: 0.0.1
`
	if got := ChartName(templated); got != "{{ chart_name }}" {
		t.Errorf("ChartName through a template = %q, want the placeholder verbatim", got)
	}
	if got := ChartVersion(templated); got != "0.0.1" {
		t.Errorf("ChartVersion = %q", got)
	}
}

func TestMissingKeysAreEmptyNotAnError(t *testing.T) {
	// Callers branch on "" — a new chart with no version yet is a legitimate state
	// (the version guard exempts charts absent at the PR base).
	if got := ChartVersion("name: x\n"); got != "" {
		t.Errorf("absent version = %q, want empty", got)
	}
	if got := ChartName(""); got != "" {
		t.Errorf("empty input = %q, want empty", got)
	}
}

func TestLeadingIndent(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"no indent", 0},
		{"  two", 2},
		{"\tone tab", 1},
		{"", 0},
	} {
		if got := LeadingIndent(tc.in); got != tc.want {
			t.Errorf("LeadingIndent(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// SiblingValue scans BOTH ways from a matched line for a key at the same indent,
// because a pin's `chart:` and `version:` appear in either order in the wild.
func TestSiblingValueScansBothDirections(t *testing.T) {
	lines := []string{
		"dependencies:",
		"  - name: platform-harbor",
		"    version: 1.4.2",
		"    repository: oci://example",
	}
	if got := SiblingValue(lines, 2, "    ", "repository"); got != "oci://example" {
		t.Errorf("forward scan = %q", got)
	}
	if got := SiblingValue(lines, 3, "    ", "version"); got != "1.4.2" {
		t.Errorf("backward scan = %q", got)
	}
	// A key at a DIFFERENT indent belongs to another entry; matching it would
	// attribute one chart's version to another.
	if got := SiblingValue(lines, 2, "    ", "dependencies"); got != "" {
		t.Errorf("cross-indent match = %q, want empty", got)
	}
}
