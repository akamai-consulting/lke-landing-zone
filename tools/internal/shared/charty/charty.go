// Package charty reads scalars out of Helm chart YAML by scanning lines.
//
// LINE-SCANNING RATHER THAN A YAML PARSE, deliberately, and that is why it is
// worth its own package rather than a call to yaml.Unmarshal at each caller. The
// guards that use this read Chart.yaml and values.yaml files that may be
// TEMPLATES -- copier placeholders, Helm expressions -- which a real parser
// rejects outright. Reading one field out of a file that is not valid YAML yet is
// the whole job.
//
// IT CAME FROM internal/extensions/chartguard, which is the guard. Two peers were
// importing that guard to read a chart's name or version, which is not a guarding
// concern; chartguard was simply the first thing that needed to.
package charty

import (
	"strings"
)

// ChartVersion extracts the top-level `version:` value from Chart.yaml content,
// or "" when absent. Only column-0 `version:` matches, so nested keys and
// `appVersion:` are not mistaken for it.
func ChartVersion(chartYAML string) string { return chartScalar(chartYAML, "version:") }

// chartScalar reads a column-0 `<key> <value>` scalar out of Chart.yaml, with
// surrounding quotes stripped.
//
// The quote stripping is the point: `version: "0.1.11"` is valid YAML, and the
// PIN side of every comparison already strips quotes (extractChartPins,
// siblingValue). Without it here, quoting a chart version makes chart-pin-guard
// compare `"0.1.11"` against `0.1.11` and report a drift that does not exist —
// a spurious failure on a legal edit. Latent today (no chart is quoted), and
// cheaper to make symmetric than to discover.
//
// Column-0 only, so nested keys and `appVersion:` are never mistaken for it.
func chartScalar(chartYAML, key string) string {
	for _, line := range strings.Split(chartYAML, "\n") {
		if strings.HasPrefix(line, key) {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, key)), `"'`)
		}
	}
	return ""
}

// ChartName extracts the top-level `name:` value from Chart.yaml content, or ""
// when absent. Mirrors ChartVersion (ci_chart_guard.go): only a column-0 key
// matches, so nested `name:` fields are not picked up.
func ChartName(chartYAML string) string { return chartScalar(chartYAML, "name:") }

// SiblingValue reads a key from the same YAML block as line idx — scanning both
// directions and stopping at a dedent.
//
// EXPORTED because `llz ci chart-publish-check` asks the same question about the
// same files and stayed in internal/cli (it is release-publish's territory, not
// this extension's). Two scanners disagreeing about what "the same block" means is
// how a pin gets read from the wrong chart.
// siblingValue returns the value of `<indent>key: <value>` in the contiguous block
// around idx at exactly the given indentation, scanning both directions and
// stopping at the first line that dedents below indent.
func SiblingValue(lines []string, idx int, indent, key string) string {
	want := len(indent)
	prefix := indent + key + ":"
	scan := func(step int) string {
		for j := idx + step; j >= 0 && j < len(lines); j += step {
			ln := lines[j]
			if strings.TrimSpace(ln) == "" {
				continue // blank lines don't break a block
			}
			if LeadingIndent(ln) < want {
				return "" // dedented out of the source block
			}
			if LeadingIndent(ln) == want && strings.HasPrefix(ln, prefix) {
				return strings.Trim(strings.TrimSpace(strings.TrimPrefix(ln, prefix)), `"'`)
			}
		}
		return ""
	}
	if v := scan(-1); v != "" {
		return v
	}
	return scan(1)
}

// leadingIndent counts the leading spaces/tabs of a line.
func LeadingIndent(s string) int {
	return len(s) - len(strings.TrimLeft(s, " \t"))
}
