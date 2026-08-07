package assertsecrets

// cobra_deps.go — the shell-out seam the moved commands reach through.
//
// execOutput delegates to kubectlprobe.Exec through a CLOSURE, never by
// assignment: an assignment snapshots whatever the seam pointed at when this
// package initialised, which defeats a test that swaps it later. Eleven other
// packages carry the identical three lines.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"

func execOutput(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }

func execCombined(name string, args ...string) string { return kubectlprobe.Combined(name, args...) }
