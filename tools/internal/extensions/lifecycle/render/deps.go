package render

// deps.go — the localisable edges.

import (
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// execLookPath goes through kubectlprobe's ONE LookPath seam.
func execLookPath(file string) (string, error) { return kubectlprobe.LookPathFn(file) }

// first3 was a one-line alias in scaffold.go for the region-shortening rule that
// internal/linode owns. Calling through to the owner rather than copying the alias
// keeps one definition of what "short region" means.
func first3(s string) string { return linode.RegionShort(s) }

// indent is copied, and package main keeps its copy too. `indent` is far too
// common a word to reach across a package boundary for — a rename that tried
// collided with a local variable of the same name in ci_chart_publish_check.go.
func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}
