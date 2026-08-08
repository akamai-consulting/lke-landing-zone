package kubectlprobe

// shellout_test.go — the two shell-out defaults, which arrived here from package
// main and brought no tests with them because package main had none for them
// either: they were `var`s that every test in sight replaced with a stub.
//
// They are worth pinning precisely because they are the fallback. Eleven packages
// delegate to Exec, and the one behaviour that distinguishes it from a plain
// .Output() — attaching stderr to the error — is invisible until something fails
// in production.

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestExecAttachesStderr is the regression the wrapper exists for. Without it a
// failure reads "exit status 1" and the tool's own diagnosis is discarded.
func TestExecAttachesStderr(t *testing.T) {
	_, err := Exec("sh", "-c", "echo the-real-reason >&2; exit 3")
	if err == nil {
		t.Fatal("a failing command returned no error")
	}
	if !strings.Contains(err.Error(), "the-real-reason") {
		t.Errorf("stderr was discarded: %v", err)
	}
	// %w, not %v: ErrText and ClassifyErr both reach for *exec.ExitError, and a
	// flattened error would silently downgrade every Absent verdict to Unknown.
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("wrapping broke errors.As: %v", err)
	}
}

// A tool that fails silently has nothing to attach, and appending an empty
// stderr would leave a trailing ": " on every such message.
func TestExecLeavesASilentFailureAlone(t *testing.T) {
	_, err := Exec("sh", "-c", "exit 4")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.HasSuffix(err.Error(), ": ") {
		t.Errorf("empty stderr was appended anyway: %q", err.Error())
	}
}

func TestExecReturnsStdoutOnSuccess(t *testing.T) {
	out, err := Exec("sh", "-c", "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "hello" {
		t.Errorf("stdout = %q", out)
	}
}

// Combined's whole job is the two things Exec drops: stderr, and output from a
// command that exited non-zero.
func TestCombinedKeepsStderrAndIgnoresExitStatus(t *testing.T) {
	got := Combined("sh", "-c", "echo out; echo err >&2; exit 1")
	for _, want := range []string{"out", "err"} {
		if !strings.Contains(got, want) {
			t.Errorf("combined output missing %q: %q", want, got)
		}
	}
}
