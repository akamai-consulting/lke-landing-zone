package seedspecial

// deps.go — the one edge this package could not bring with it.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"

// execOutput delegates to kubectlprobe.Exec through a CLOSURE rather than by
// assignment: a direct assignment would snapshot whatever kubectlprobe.Exec
// pointed at when this package initialised, freezing it before any test could
// swap it. That bug has cost this campaign three times.
func execOutput(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }
