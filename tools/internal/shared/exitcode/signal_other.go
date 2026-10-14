//go:build !unix

package exitcode

import "os/exec"

// signalExit has no portable answer off unix; FromExec then keeps the original
// error, which still reports a failure — just without the 128+N refinement.
func signalExit(*exec.ExitError) (int, bool) { return 0, false }
