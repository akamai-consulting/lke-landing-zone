package buildpreflight

// deps.go — the two edges this package could not bring with it.

import (
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// forgeHandle is this package's ONE forge seam, and it comes from the DECLARATION
// rather than from a package var.
//
// It used to be `execOutput`, a closure over kubectlprobe.Exec — which despite the
// name is the tree's general process runner, so this package held an unconstrained
// launcher in order to make one read-only `gh api` call. The binding declares
// cloud-read and read-repo; capability.For turns exactly that into a handle that
// permits `gh api repos/<r>` and refuses `gh api -X PUT`, `gh secret set` and
// anything unclassified.
//
// Reached at CALL TIME through a function rather than stored, for the reason the
// old comment gave: a stored value snapshots whatever the seam pointed at when
// this package initialised, which is the capture bug this campaign has on record.
func forgeHandle() capability.Forge { return capability.For(readBinding()).Forge }

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
