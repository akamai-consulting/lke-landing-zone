package exitcode

import (
	"bytes"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// runExit runs a shell that exits with the given status, returning the error
// exactly as a caller's cmd.Run() would produce it.
func runExit(t *testing.T, script string) error {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("a shell is required to produce a real *exec.ExitError")
	}
	return exec.Command("sh", "-c", script).Run()
}

// THE DEFECT THIS PINS was silent and scriptable: every error reaching Main()
// became exit 1, so `tofu plan -detailed-exitcode` returning 2 for "changes
// pending" arrived as 1, indistinguishable from a crash.
func TestChildStatusSurvives(t *testing.T) {
	for _, code := range []int{1, 2, 42} {
		err := FromExec(runExit(t, "exit "+strconv.Itoa(code)))
		var p *Passthrough
		if !errors.As(err, &p) {
			t.Fatalf("exit %d: want a Passthrough, got %T (%v)", code, err, err)
		}
		if p.Code != code {
			t.Errorf("exit %d reported as %d", code, p.Code)
		}
		if got := Report(&bytes.Buffer{}, err); got != code {
			t.Errorf("Report returned %d for a child that exited %d", got, code)
		}
	}
}

// A child that succeeded is not an error and must not travel as one — a
// Passthrough{Code: 0} would make `llz tofu -- fmt` exit 0 through the error
// path, which works by accident and stops working the moment anything inspects
// the error.
func TestSuccessIsNotAnError(t *testing.T) {
	if err := FromExec(nil); err != nil {
		t.Errorf("FromExec(nil) = %v, want nil", err)
	}
	if got := Report(&bytes.Buffer{}, nil); got != 0 {
		t.Errorf("Report(nil) = %d, want 0", got)
	}
}

// The child wrote to the same stderr the operator is reading, and OpenTofu's
// diagnostics are better than anything llz could add. "llz: exit status 2" after
// them says nothing and pushes the real message up the screen.
func TestReportStaysSilentForAChildThatAlreadySpoke(t *testing.T) {
	var b bytes.Buffer
	if got := Report(&b, &Passthrough{Code: 3}); got != 3 {
		t.Errorf("Report = %d, want 3", got)
	}
	if b.Len() != 0 {
		t.Errorf("Report added noise after the child's own output: %q", b.String())
	}
}

// llz's OWN failures still need their message — losing those would trade one
// silent failure for another.
func TestReportStillPrintsLLZsOwnErrors(t *testing.T) {
	var b bytes.Buffer
	if got := Report(&b, errors.New("not inside an instance checkout")); got != 1 {
		t.Errorf("Report = %d, want 1", got)
	}
	if !strings.Contains(b.String(), "not inside an instance checkout") {
		t.Errorf("llz's own error was swallowed: %q", b.String())
	}
}

// A missing binary is llz's failure to report, not a child's status: there is no
// child and no status, so the message must survive.
func TestNonExitErrorsPassThroughUnchanged(t *testing.T) {
	err := FromExec(exec.Command("llz-no-such-binary-anywhere").Run())
	var p *Passthrough
	if errors.As(err, &p) {
		t.Fatalf("a failed exec became a child status: %v", err)
	}
	var b bytes.Buffer
	if got := Report(&b, err); got != 1 || b.Len() == 0 {
		t.Errorf("want a printed error and exit 1, got %d / %q", got, b.String())
	}
}

// Killed by a signal: ExitCode() is -1, which nothing can exit with. 128+N is
// what every shell reports, so a Ctrl-C during `llz tofu -- apply` surfaces as
// "the operator stopped it" rather than a generic failure.
func TestSignalDeathBecomes128PlusSignal(t *testing.T) {
	err := FromExec(runExit(t, "kill -TERM $$"))
	var p *Passthrough
	if !errors.As(err, &p) {
		t.Fatalf("want a Passthrough for a signalled child, got %T (%v)", err, err)
	}
	if p.Code != 143 { // 128 + SIGTERM(15)
		t.Errorf("signalled child reported as %d, want 143", p.Code)
	}
}
