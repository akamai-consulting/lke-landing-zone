package reconciler

// deps.go — the two edges this package could not bring with it.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"

// Version is the build version stamped into the llz_reconcile_build_info gauge.
//
// INJECTED RATHER THAN READ. `version` is a package-main var set by -ldflags at
// link time, and a build stamp is exactly the kind of thing that must not have two
// sources: an internal package that defined its own would report "dev" from a
// released binary and nobody would notice, because a build_info gauge is only ever
// read when something has already gone wrong.
var Version = "dev"

// execOutput is the stdout-only, error-gated exec, delegating to kubectlprobe
// through a CLOSURE rather than by assignment — a direct assignment would snapshot
// whatever kubectlprobe.Exec pointed at when this package initialised, freezing it
// before any test could swap it. That bug has cost this campaign three times.
func execOutput(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }
