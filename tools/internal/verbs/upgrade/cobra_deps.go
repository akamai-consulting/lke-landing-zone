package upgrade

// cobra_deps.go — the shell-out seams the upgrade-test gate reaches through.
//
// execOutput/execLookPath delegate to kubectlprobe through CLOSURES, never by
// assignment: an assignment snapshots whatever the seam pointed at when this
// package initialised, which defeats a test that swaps it later.
//
// gitcmd.Output is a four-line local copy. The original is in hooks.go, which is
// package main's and is not moving; `git -C dir` is the whole implementation.

import (
	"regexp"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

func execLookPath(file string) (string, error) { return kubectlprobe.LookPathFn(file) }

// hexSHARe matches a git SHA, short or full. A third copy — internal/templatecommit
// has one and so did ci_assert_image_fresh.go — and deliberately so: it is one
// line, and a shared regexp package would exist only to avoid typing it.
var hexSHARe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
