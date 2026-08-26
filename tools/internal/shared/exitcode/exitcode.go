// Package exitcode carries a child process's exit status out through llz's own.
//
// A passthrough's exit status is part of its contract: `tofu plan
// -detailed-exitcode` returns 2 for "changes pending", which `llz ci tf-plan`
// gates a workflow on, and flattening every error to 1 makes that
// indistinguishable from a crash. The other half is silence — when the child has
// already printed its own diagnostics, appending "llz: exit status 2" only buries
// them, so a Passthrough is reported by its CODE alone.
//
// It lives here rather than in tofudriver because the decision belongs wherever
// an error becomes a status, which is main's.
package exitcode

import (
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// Passthrough is a child process's exit status, on its way out through ours.
//
// It carries no message on purpose: the child wrote to the same stderr the
// operator is reading. Error() exists to satisfy the interface.
type Passthrough struct {
	// Code is the child's exit status. Never 0 — a zero status is not an error
	// and must not travel as one.
	Code int
}

func (e *Passthrough) Error() string {
	return fmt.Sprintf("child process exited with status %d", e.Code)
}

// FromExec converts a failed exec.Cmd.Run into a Passthrough, so the child's
// status survives the trip through cobra.
//
// A non-ExitError — the binary is missing, the fork failed — is returned
// unchanged: that is llz's own failure to report, not the child's status, and it
// needs the message.
func FromExec(err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code > 0 {
			return &Passthrough{Code: code}
		}
		// ExitCode() cannot express death by signal. 128+signal is the shell's
		// convention; falling through to 1 would report a SIGINT as an ordinary
		// failure.
		if status, ok := signalExit(ee); ok {
			return &Passthrough{Code: status}
		}
	}
	return err
}

// Report writes err unless the child already spoke for itself, and returns the
// status the process should exit with.
func Report(w io.Writer, err error) int {
	if err == nil {
		return 0
	}
	var p *Passthrough
	if errors.As(err, &p) {
		return p.Code
	}
	fmt.Fprintln(w, color.Red("llz:"), err)
	return 1
}
