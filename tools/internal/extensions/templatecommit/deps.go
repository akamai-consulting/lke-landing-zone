package templatecommit

// deps.go — the three edges this package could not bring with it.

import (
	"regexp"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// execOutput delegates to kubectlprobe.Exec through a CLOSURE, never by
// assignment. Nothing here runs kubectl — it runs git and gh — but that seam has
// always taken the binary as its first argument and it is the ONE seam.
func execOutput(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }

// firstNonEmpty is copied, not shared. Sixteenth package in this campaign to keep
// its own three lines.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// hexSHARe matches a git SHA, short or full. Copied from ci_assert_image_fresh.go:
// a compiled regexp with two callers is not worth a package boundary, and putting
// it behind one would mean importing an assert lane to recognise a SHA.
var hexSHARe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// Version is the baked llz build stamp, installed by package main from its
// -ldflags value. A fourth consumer of the same seam (reconciler, selfupgrade
// and copier take it too) rather than a re-declared `var version`: the whole
// point of assert-image-fresh is comparing THIS binary's stamp against the
// instance's pin, so a package-local default would compare a constant to itself.
var Version = "dev"
