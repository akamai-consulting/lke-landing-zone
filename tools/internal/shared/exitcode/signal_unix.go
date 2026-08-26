//go:build unix

package exitcode

// signal_unix.go — the 128+N half of FromExec. ExitCode() returns -1 for a
// signalled process, which is not a status anything can exit with; 128+signal is
// what every shell reports, so a Ctrl-C during an apply surfaces as 130 rather
// than a generic 1. Behind a build tag because syscall.WaitStatus is not portable.

import (
	"os/exec"
	"syscall"
)

func signalExit(ee *exec.ExitError) (int, bool) {
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return 128 + int(ws.Signal()), true
}
