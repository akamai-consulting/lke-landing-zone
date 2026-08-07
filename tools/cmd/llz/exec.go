package main

// exec.go — package main's three shell-out helpers, now delegating closures.
//
// THIS FILE USED TO HOLD THE IMPLEMENTATIONS, and an init() that overwrote
// kubectlprobe.Exec with them at startup. That made package main the OWNER of a
// seam eleven other packages delegate to, and it worked only while the llz binary
// was the thing running: nothing else executes main's init, so every test in
// every one of those packages got kubectlprobe's plainer default instead. The
// stderr-attaching body now lives there as the default, and this file is what the
// other eleven already look like.
//
// KEPT AS CLOSURES RATHER THAN DELETED. Rewriting ~47 call sites to say
// kubectlprobe.Exec would be a large diff for no behaviour change, and — after
// the prose damage a word-boundary rename shipped three moves ago — a large diff
// across files that also mention these names in help strings is exactly the shape
// worth not taking. Closures, never assignment: an assignment snapshots whatever
// the seam pointed at when this package initialised, which is the swap-at-call-
// time property every test here relies on.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

// execOutput runs name with args and returns its standard output. Stderr is
// attached to the error on failure; see kubectlprobe.Exec for why.
func execOutput(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }

// execCombined returns combined stdout+stderr as a string, ignoring exit status.
func execCombined(name string, args ...string) string { return kubectlprobe.Combined(name, args...) }

// execLookPath reports a binary's location on PATH.
func execLookPath(file string) (string, error) { return kubectlprobe.LookPathFn(file) }
