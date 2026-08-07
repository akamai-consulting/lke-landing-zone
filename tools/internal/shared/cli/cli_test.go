package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
)

func TestEnvInt(t *testing.T) {
	t.Setenv("X_INT", "30")
	if got := EnvInt("X_INT", 90); got != 30 {
		t.Errorf("EnvInt(set) = %d, want 30", got)
	}
	if got := EnvInt("X_UNSET_INT", 90); got != 90 {
		t.Errorf("EnvInt(unset) = %d, want 90 (default)", got)
	}
	t.Setenv("X_INT", "not-a-number")
	if got := EnvInt("X_INT", 90); got != 90 {
		t.Errorf("EnvInt(invalid) = %d, want 90 (default)", got)
	}
}

func TestEnvBool(t *testing.T) {
	t.Setenv("X_BOOL", "true")
	if !EnvBool("X_BOOL", false) {
		t.Error("EnvBool(true) = false, want true")
	}
	if EnvBool("X_UNSET_BOOL", false) {
		t.Error("EnvBool(unset) = true, want false (default)")
	}
	t.Setenv("X_BOOL", "garbage")
	if EnvBool("X_BOOL", false) {
		t.Error("EnvBool(invalid) = true, want false (default)")
	}
}

func TestMustUint(t *testing.T) {
	if got := MustUint("613260"); got != 613260 {
		t.Errorf("MustUint(\"613260\") = %d, want 613260", got)
	}
}

func TestAsUint64(t *testing.T) {
	if got, ok := AsUint64(json.Number("123")); !ok || got != 123 {
		t.Errorf("AsUint64(json.Number) = (%d, %v), want (123, true)", got, ok)
	}
	if _, ok := AsUint64("123"); ok {
		t.Error("AsUint64(string) should report not-ok")
	}
	if _, ok := AsUint64(json.Number("nan")); ok {
		t.Error("AsUint64(bad number) should report not-ok")
	}
	if _, ok := AsUint64(nil); ok {
		t.Error("AsUint64(nil) should report not-ok")
	}
}

func TestAsString(t *testing.T) {
	if got := AsString("hi"); got != "hi" {
		t.Errorf("AsString(string) = %q, want hi", got)
	}
	if got := AsString(7); got != "" {
		t.Errorf("AsString(non-string) = %q, want empty", got)
	}
	if got := AsString(nil); got != "" {
		t.Errorf("AsString(nil) = %q, want empty", got)
	}
}

func TestPrintRecord(t *testing.T) {
	out := captureStdout(t, func() {
		if err := PrintRecord(map[string]any{"event": "x", "n": 1}); err != nil {
			t.Fatalf("PrintRecord: %v", err)
		}
	})
	// One JSON line; keys are sorted by encoding/json.
	if out != `{"event":"x","n":1}`+"\n" {
		t.Errorf("PrintRecord output = %q", out)
	}
}

// ── os.Exit(2) paths, exercised via a forked test binary ─────────────────────
//
// MustUint calls os.Exit on malformed input, which would abort the
// test process. The canonical Go pattern re-execs this binary running only the
// target test with CLI_FORK set, then asserts the child exited with status 2.

func TestMustUintExitsOnBadInput(t *testing.T) {
	if os.Getenv("CLI_FORK") == "mustuint" {
		MustUint("-1")
		return
	}
	assertExit2(t, "TestMustUintExitsOnBadInput", "mustuint")
}

func assertExit2(t *testing.T, testName, mode string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$")
	cmd.Env = append(os.Environ(), "CLI_FORK="+mode)
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 2 {
		return
	}
	t.Fatalf("%s child err = %v, want exit status 2", mode, err)
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
