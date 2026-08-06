package main

// indent is package main's copy of the five-line helper internal/configreadiness
// also has. Copied rather than imported because `indent` is far too common a word
// to reach across a package boundary for — the rename that tried collided with a
// local variable of the same name in ci_chart_publish_check.go.

import "strings"

func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}
