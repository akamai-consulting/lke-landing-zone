package baoread

// kvput_test.go — the write half, tested where it now lives.
//
// It arrived from cmd/llz/ci_harbor.go with no tests of its own: every caller
// stubbed baoread.KVPut, so the implementation behind the seam was exercised by
// nothing. That is the ordinary fate of a function that lives one package away
// from the seam it satisfies.

import (
	"strings"
	"testing"
)

func TestKVPutRefusesWithoutARootToken(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "")
	called := false
	orig := ExecFn
	ExecFn = func(string, string, string, ...string) (string, string, error) { called = true; return "", "", nil }
	t.Cleanup(func() { ExecFn = orig })

	err := kvPut("secret/x", map[string]string{"a": "1"})
	if err == nil || !strings.Contains(err.Error(), "OPENBAO_ROOT_TOKEN") {
		t.Fatalf("must name the missing token, got %v", err)
	}
	if called {
		t.Error("must not exec without a token — this is what makes a REAL default safe here")
	}
}

func TestKVPutSortsFieldsForADeterministicArgv(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "tok")
	var gotPod, gotToken string
	var gotArgs []string
	orig := ExecFn
	ExecFn = func(pod, token, _ string, args ...string) (string, string, error) {
		gotPod, gotToken, gotArgs = pod, token, args
		return "", "", nil
	}
	t.Cleanup(func() { ExecFn = orig })

	if err := kvPut("secret/harbor/robot", map[string]string{"zeta": "3", "alpha": "1", "mid": "2"}); err != nil {
		t.Fatal(err)
	}
	if gotPod != RootPod || gotToken != "tok" {
		t.Errorf("pod/token = %q/%q, want %q/%q", gotPod, gotToken, RootPod, "tok")
	}
	want := []string{"kv", "put", "secret/harbor/robot", "alpha=1", "mid=2", "zeta=3"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v — map iteration order is random, so the sort is what makes\n"+
			"a re-run of the same seed produce the same command", gotArgs, want)
	}
}

func TestKVPutReportsStderrInPreferenceToStdout(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "tok")
	orig := ExecFn
	ExecFn = func(string, string, string, ...string) (string, string, error) {
		return "some stdout", "permission denied", errFake{}
	}
	t.Cleanup(func() { ExecFn = orig })

	err := kvPut("secret/x", map[string]string{"a": "1"})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("stderr is the diagnostic that matters, got %v", err)
	}
	if strings.Contains(err.Error(), "some stdout") {
		t.Error("stdout must not drown the stderr line")
	}
}

type errFake struct{}

func (errFake) Error() string { return "exit status 2" }
