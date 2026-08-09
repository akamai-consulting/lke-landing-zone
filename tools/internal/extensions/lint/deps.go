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
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/sustain"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// SustainDeps is installed by package main before any command runs.
var SustainDeps func() sustain.Deps

func execLookPath(file string) (string, error) { return kubectlprobe.LookPathFn(file) }
