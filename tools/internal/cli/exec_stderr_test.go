package cli

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// .Output() captures stdout only, and kubectl/bao/tofu all write their diagnosis
// to STDERR — so a failure used to arrive as a bare "exit status 1" with the cause
// discarded. A real e2e failure read:
//
//	llz: patch llz-openbao/platform-openbao hostAliases: exit status 1:
//
// with nothing after the colon, and could not be diagnosed from CI at all.
func TestExecOutputSurfacesStderr(t *testing.T) {
	_, err := execOutput("sh", "-c", "echo to-stderr >&2; exit 3")
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	if !strings.Contains(err.Error(), "to-stderr") {
		t.Errorf("stderr must appear in the error, got %q — without it a CI failure has no cause", err)
	}
	// %w preserved: callers still classify on the original.
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Error("the wrapped error must still unwrap to *exec.ExitError")
	}
}

// A tool that fails silently must not gain a spurious suffix.
func TestExecOutputLeavesSilentFailuresAlone(t *testing.T) {
	_, err := execOutput("sh", "-c", "exit 4")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.HasSuffix(strings.TrimSpace(err.Error()), ":") {
		t.Errorf("no stderr should mean no trailing colon, got %q", err)
	}
}
