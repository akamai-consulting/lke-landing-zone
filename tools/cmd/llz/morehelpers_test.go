package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote — these helpers print a human report we don't want in test output.
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
	var b strings.Builder
	if _, err := io.Copy(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 3); got != "hel" {
		t.Errorf("truncate(hello,3) = %q, want hel", got)
	}
	if got := truncate("hi", 5); got != "hi" {
		t.Errorf("truncate(hi,5) = %q, want hi", got)
	}
}

func TestIsMissingBinary(t *testing.T) {
	if !isMissingBinary(&exec.Error{Name: "tflint", Err: exec.ErrNotFound}) {
		t.Error("isMissingBinary(*exec.Error) = false, want true")
	}
	if isMissingBinary(errors.New("some other error")) {
		t.Error("isMissingBinary(generic) = true, want false")
	}
}

func TestPrompt(t *testing.T) {
	var got string
	out := captureStdout(t, func() {
		got = prompt(bufio.NewScanner(strings.NewReader("  trimmed \n")), "Token")
	})
	if got != "trimmed" {
		t.Errorf("prompt = %q, want trimmed", got)
	}
	if !strings.Contains(out, "Token") {
		t.Errorf("prompt did not print its label: %q", out)
	}
	// Empty input -> empty answer.
	captureStdout(t, func() {
		if v := prompt(bufio.NewScanner(strings.NewReader("")), "x"); v != "" {
			t.Errorf("prompt(empty) = %q, want empty", v)
		}
	})
}

func TestTfvarsPaths(t *testing.T) {
	paths := tfvarsPaths("/tf", "dev")
	if len(paths) != len(tfRoots) {
		t.Fatalf("tfvarsPaths returned %d paths, want %d", len(paths), len(tfRoots))
	}
	if !containsString(paths, "/tf/cluster/dev.tfvars") {
		t.Errorf("tfvarsPaths missing /tf/cluster/dev.tfvars: %v", paths)
	}
}

func TestSatisfied(t *testing.T) {
	vars := map[string]string{"VAR_A": "1"}
	secrets := map[string]string{"SEC_A": "x"}
	st := liveState{repoVars: map[string]string{"VAR_B": "2"}, repoSecrets: map[string]bool{"SEC_B": true}}

	cases := []struct {
		name string
		req  requirement
		want bool
	}{
		{"var in cache", requirement{Name: "VAR_A"}, true},
		{"secret in cache", requirement{Name: "SEC_A", Secret: true}, true},
		{"var on github", requirement{Name: "VAR_B"}, true},
		{"secret on github", requirement{Name: "SEC_B", Secret: true}, true},
		{"absent var", requirement{Name: "VAR_X"}, false},
		{"absent secret", requirement{Name: "SEC_X", Secret: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := satisfied(tc.req, secrets, vars, st); got != tc.want {
				t.Errorf("satisfied(%+v) = %v, want %v", tc.req, got, tc.want)
			}
		})
	}
}

func TestPrepopulateVars(t *testing.T) {
	reqs := []requirement{
		{Name: "FROM_INSTANCE"},
		{Name: "FROM_TEMPLATE", Template: true},
		{Name: "ALREADY_SET"},
		{Name: "A_SECRET", Secret: true}, // skipped: secrets aren't prepopulated
		{Name: "UNAVAILABLE"},
	}
	vars := map[string]string{"ALREADY_SET": "keep"}
	instance := liveState{repoVars: map[string]string{"FROM_INSTANCE": "iv"}}
	template := liveState{repoVars: map[string]string{"FROM_TEMPLATE": "tv"}}

	n := prepopulateVars(vars, reqs, instance, template)
	if n != 2 {
		t.Errorf("prepopulateVars filled %d, want 2", n)
	}
	if vars["FROM_INSTANCE"] != "iv" || vars["FROM_TEMPLATE"] != "tv" {
		t.Errorf("prepopulated values wrong: %v", vars)
	}
	if vars["ALREADY_SET"] != "keep" {
		t.Errorf("prepopulateVars clobbered an existing value: %q", vars["ALREADY_SET"])
	}
	if _, ok := vars["A_SECRET"]; ok {
		t.Error("prepopulateVars should not fill secrets")
	}
}

func TestReportReadiness(t *testing.T) {
	reqs := []requirement{
		{Name: "OK_VAR", Required: true},        // on github -> not missing
		{Name: "CACHED_VAR", Required: true},    // cached -> still missing (not yet pushed)
		{Name: "MISSING_VAR", Required: true},   // missing
		{Name: "OPTIONAL_VAR", Required: false}, // missing but optional -> not reported
	}
	vars := map[string]string{"CACHED_VAR": "v"}
	secrets := map[string]string{}
	instance := liveState{repoVars: map[string]string{"OK_VAR": "set"}}
	template := liveState{}

	var missing []string
	out := captureStdout(t, func() {
		missing = reportReadiness(reqs, secrets, vars, instance, template, nil)
	})
	if !containsString(missing, "CACHED_VAR") || !containsString(missing, "MISSING_VAR") {
		t.Errorf("missing = %v, want CACHED_VAR and MISSING_VAR", missing)
	}
	if containsString(missing, "OK_VAR") || containsString(missing, "OPTIONAL_VAR") {
		t.Errorf("missing should exclude OK_VAR and OPTIONAL_VAR: %v", missing)
	}
	if !strings.Contains(out, "NAME") {
		t.Errorf("reportReadiness did not print a header: %q", out)
	}
}
