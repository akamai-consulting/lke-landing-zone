package buildpreflight

// deps.go — the two edges this package could not bring with it.

import (
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// execOutput delegates to kubectlprobe.Exec through a CLOSURE, never by
// assignment. The name is inherited and the package it points at is named for its
// first callers; what matters is that this is the ONE seam, reached at call time.
func execOutput(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }

// existingPaths filters a list down to the paths that exist. Copied from
// scaffold.go rather than shared: nine lines with two callers, and a package
// boundary for a Stat loop costs more to read than it saves.
func existingPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}
