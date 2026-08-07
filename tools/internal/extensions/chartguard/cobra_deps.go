package chartguard

// cobra_deps.go — the shell-out seam the moved commands reach through.
//
// execOutput delegates to kubectlprobe.Exec through a CLOSURE, never by
// assignment: an assignment snapshots whatever the seam pointed at when this
// package initialised, which defeats a test that swaps it later. Eleven other
// packages carry the identical three lines.

import (
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

func execOutput(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }

// gitOutput runs git in dir and returns trimmed stdout. A local four-line copy
// rather than a shared helper: `git -C dir` is the whole implementation, and the
// original lives in hooks.go, which is main's and is not moving.
func gitOutput(dir string, args ...string) (string, error) {
	out, err := execOutput("git", append([]string{"-C", dir}, args...)...)
	return strings.TrimSpace(string(out)), err
}
