package lint

// deps.go — the two seams and one closure this package reaches the outside through.
//
// SustainDeps is INJECTED because assembling sustain.Deps needs
// lockableScaffoldFiles, which ADR 0014 pins to .template-manifest in package
// main and which therefore cannot move. internal/extensions/upgrade takes the
// identical seam for the identical reason; two installs is the price of not
// hoisting a deps assembler out of the layer that owns it.
//
// execOutput/execLookPath delegate to kubectlprobe through CLOSURES, never by
// assignment: an assignment snapshots whatever the seam pointed at when this
// package initialised, which defeats a test that swaps it later.

import (
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/sustain"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// SustainDeps is installed by package main before any command runs.
var SustainDeps func() sustain.Deps

func execOutput(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }

func execLookPath(file string) (string, error) { return kubectlprobe.LookPathFn(file) }

// gitOutput runs git in dir and returns trimmed stdout. A four-line local copy;
// the original is in hooks.go, which is package main's and is not moving.
func gitOutput(dir string, args ...string) (string, error) {
	out, err := execOutput("git", append([]string{"-C", dir}, args...)...)
	return strings.TrimSpace(string(out)), err
}
