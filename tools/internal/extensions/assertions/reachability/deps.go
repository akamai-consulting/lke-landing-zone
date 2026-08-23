package reachability

// deps.go — the three edges this family could not bring with it.

import (
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// execOutput delegates to kubectlprobe.Exec through a CLOSURE, never by
// assignment: a direct assignment would snapshot whatever kubectlprobe.Exec
// pointed at when this package initialised, freezing it before any test could
// swap it. That bug has recurred.
func execOutput(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }

// execLookPath goes through kubectlprobe's seam so a preflight test can answer for
// a tool the developer's machine happens to have installed — the exact failure
// mode a preflight has, which is passing on the laptop that wrote it.
func execLookPath(file string) (string, error) { return kubectlprobe.LookPathFn(file) }

// report prints one ✓/✗ line. COPIED from wizard.go, not shared, following the
// call this tree already made and wrote down in internal/kyverno: "printers
// and fixtures travel by copy — the same call made for firstNonEmpty, orAll and
// report". Exporting it would put a symbol in a package's API whose only job is
// to be reachable from the other side.
func report(name string, ok bool) {
	mark := color.Red("✗")
	if ok {
		mark = color.Green("✓")
	}
	fmt.Printf("  %s  %s\n", mark, name)
}
