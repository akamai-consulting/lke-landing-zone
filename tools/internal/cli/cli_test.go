package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
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

func TestEnvOr(t *testing.T) {
	t.Setenv("X_STR", "value")
	if got := EnvOr("X_STR", "def"); got != "value" {
		t.Errorf("EnvOr(set) = %q, want value", got)
	}
	if got := EnvOr("X_UNSET_STR", "def"); got != "def" {
		t.Errorf("EnvOr(unset) = %q, want def", got)
	}
	t.Setenv("X_STR", "")
	if got := EnvOr("X_STR", "def"); got != "def" {
		t.Errorf("EnvOr(empty) = %q, want def", got)
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

func TestMustInt(t *testing.T) {
	if got := MustInt("42"); got != 42 {
		t.Errorf("MustInt(\"42\") = %d, want 42", got)
	}
	if got := MustInt("-7"); got != -7 {
		t.Errorf("MustInt(\"-7\") = %d, want -7", got)
	}
}

func TestMustUint(t *testing.T) {
	if got := MustUint("613260"); got != 613260 {
		t.Errorf("MustUint(\"613260\") = %d, want 613260", got)
	}
}

func TestArg(t *testing.T) {
	if got := Arg([]string{"--flag", "value"}, 1); got != "value" {
		t.Errorf("Arg = %q, want value", got)
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

func TestParseRotatorArgs(t *testing.T) {
	t.Setenv("LINODE_TOKEN", "envtok")
	t.Setenv("ROTATION_APPLY", "")

	// Flag overrides the env token; subcommand is split from its rest; --apply arms.
	a := ParseRotatorArgs([]string{"--linode-token", "flagtok", "create", "--label", "x", "--apply"})
	if a.Token != "flagtok" {
		t.Errorf("Token = %q, want flagtok (flag overrides env)", a.Token)
	}
	if !a.Apply {
		t.Error("Apply = false, want true (--apply)")
	}
	if a.Sub != "create" {
		t.Errorf("Sub = %q, want create", a.Sub)
	}
	if !reflect.DeepEqual(a.Rest, []string{"--label", "x"}) {
		t.Errorf("Rest = %v, want [--label x]", a.Rest)
	}
}

func TestParseRotatorArgsDefaults(t *testing.T) {
	t.Setenv("LINODE_TOKEN", "envtok")
	t.Setenv("ROTATION_APPLY", "true")

	a := ParseRotatorArgs(nil)
	if a.Token != "envtok" {
		t.Errorf("Token = %q, want envtok (from env)", a.Token)
	}
	if !a.Apply {
		t.Error("Apply = false, want true (from ROTATION_APPLY)")
	}
	if a.Sub != "" {
		t.Errorf("Sub = %q, want empty", a.Sub)
	}
}

// ── os.Exit(2) paths, exercised via a forked test binary ─────────────────────
//
// Arg / MustInt / MustUint call os.Exit on malformed input, which would abort the
// test process. The canonical Go pattern re-execs this binary running only the
// target test with CLI_FORK set, then asserts the child exited with status 2.

func TestArgExitsPastEnd(t *testing.T) {
	if os.Getenv("CLI_FORK") == "arg" {
		Arg([]string{"--flag"}, 1) // value index past the end
		return
	}
	assertExit2(t, "TestArgExitsPastEnd", "arg")
}

func TestMustIntExitsOnBadInput(t *testing.T) {
	if os.Getenv("CLI_FORK") == "mustint" {
		MustInt("not-a-number")
		return
	}
	assertExit2(t, "TestMustIntExitsOnBadInput", "mustint")
}

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
