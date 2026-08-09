package upgrade

// cobra_deps.go — the shell-out seams the upgrade-test gate reaches through.
//
// execOutput/execLookPath delegate to kubectlprobe through CLOSURES, never by
// assignment: an assignment snapshots whatever the seam pointed at when this
// package initialised, which defeats a test that swaps it later.
//
// gitOutput is a four-line local copy. The original is in hooks.go, which is
// package main's and is not moving; `git -C dir` is the whole implementation.

import (
	"regexp"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

func execOutput(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }

func execLookPath(file string) (string, error) { return kubectlprobe.LookPathFn(file) }

func gitOutput(dir string, args ...string) (string, error) {
	out, err := execOutput("git", append([]string{"-C", dir}, args...)...)
	return strings.TrimSpace(string(out)), err
}

// hexSHARe matches a git SHA, short or full. A third copy — internal/templatecommit
// has one and so did ci_assert_image_fresh.go — and deliberately so: it is one
// line, and a shared regexp package would exist only to avoid typing it.
var hexSHARe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
