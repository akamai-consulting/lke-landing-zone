package selfupgrade

// deps.go — the two edges this package could not bring with it.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"

// execOutput delegates to kubectlprobe.Exec through a CLOSURE, never by
// assignment. Nothing here runs kubectl — it runs git and gh — but that package
// has always taken the binary as its first argument, and it is the ONE seam.
func execOutput(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }

// Version is the llz build stamp, injected by package main at init.
//
// Same call as internal/reconciler.Version, and for the same reason: `Version` is
// set by -ldflags at link time, and a package that defined its own would report
// "dev" from a released binary. Here it decides whether an update is AVAILABLE, so
// a wrong answer is not cosmetic — a stale "dev" would make every release look
// newer than the running binary, forever.
var Version = "dev"
