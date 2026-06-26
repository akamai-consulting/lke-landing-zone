package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTailLines(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"empty", "", 80, ""},
		{"zero-lines", "a\nb\n", 0, ""},
		{"negative-lines", "a\nb\n", -1, ""},
		{"fewer-than-n", "a\nb\n", 80, "a\nb"},
		{"exactly-n", "a\nb\nc\n", 3, "a\nb\nc"},
		{"more-than-n", "a\nb\nc\nd\n", 2, "c\nd"},
		{"no-trailing-newline", "a\nb\nc", 2, "b\nc"},
		// `tail -N`: a trailing newline ends the last line, it doesn't start
		// an empty one — so the last *line* here is "c", not "".
		{"trailing-newline-not-a-line", "a\nb\nc\n", 1, "c"},
		{"blank-interior-lines", "a\n\nc\n", 3, "a\n\nc"},
	}
	for _, c := range cases {
		if got := tailLines(c.s, c.n); got != c.want {
			t.Errorf("%s: tailLines(%q, %d)\n got: %q\nwant: %q", c.name, c.s, c.n, got, c.want)
		}
	}
}

func TestTFPlanSummary(t *testing.T) {
	cases := []struct {
		name  string
		title string
		out   string
		n     int
		want  string
	}{
		{"basic", "Cluster plan", "line1\nline2\n", 80,
			"### Cluster plan\n```\nline1\nline2\n```\n"},
		{"truncated-to-n", "T", "a\nb\nc\n", 2,
			"### T\n```\nb\nc\n```\n"},
		{"empty-plan-output", "T", "", 80,
			"### T\n```\n```\n"},
		{"no-trailing-newline-still-fenced", "T", "only", 80,
			"### T\n```\nonly\n```\n"},
	}
	for _, c := range cases {
		if got := tfPlanSummary(c.title, c.out, c.n); got != c.want {
			t.Errorf("%s\n got: %q\nwant: %q", c.name, got, c.want)
		}
	}
}

// stubTFPlan swaps the terraform-exec seam for the duration of the test.
func stubTFPlan(t *testing.T, fn func(io.Writer, []string) error) {
	t.Helper()
	prev := tfPlanRunFn
	tfPlanRunFn = fn
	t.Cleanup(func() { tfPlanRunFn = prev })
}

// execTFPlan runs the cobra command end-to-end with the given CLI args.
func execTFPlan(t *testing.T, args ...string) error {
	t.Helper()
	c := ciTFPlanCmd()
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	c.SetArgs(args)
	return c.Execute()
}

func TestCITFPlanSuccessWritesTeeAndSummary(t *testing.T) {
	dir := t.TempDir()
	tee := filepath.Join(dir, "plan.txt")
	summary := filepath.Join(dir, "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)

	var gotFlags []string
	stubTFPlan(t, func(w io.Writer, flags []string) error {
		gotFlags = flags
		_, err := io.WriteString(w, "Plan: 3 to add, 0 to change, 0 to destroy.\n")
		return err
	})

	if err := execTFPlan(t, "--out", tee, "--title", "Cluster plan",
		"--", "-var", "env=e2e", "-target=module.lke"); err != nil {
		t.Fatal(err)
	}

	// Flags after `--` pass through to terraform untouched.
	if want := []string{"-var", "env=e2e", "-target=module.lke"}; strings.Join(gotFlags, " ") != strings.Join(want, " ") {
		t.Errorf("tf flags: got %q want %q", gotFlags, want)
	}
	if b, err := os.ReadFile(tee); err != nil || string(b) != "Plan: 3 to add, 0 to change, 0 to destroy.\n" {
		t.Errorf("tee file: got %q, %v", b, err)
	}
	wantSummary := "### Cluster plan\n```\nPlan: 3 to add, 0 to change, 0 to destroy.\n```\n"
	if b, err := os.ReadFile(summary); err != nil || string(b) != wantSummary {
		t.Errorf("summary: got %q, %v\nwant %q", b, err, wantSummary)
	}
}

func TestCITFPlanLinesDefault80(t *testing.T) {
	dir := t.TempDir()
	summary := filepath.Join(dir, "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)

	var out strings.Builder
	for i := 1; i <= 100; i++ {
		out.WriteString("line\n")
	}
	stubTFPlan(t, func(w io.Writer, _ []string) error {
		_, err := io.WriteString(w, out.String())
		return err
	})

	if err := execTFPlan(t, "--out", filepath.Join(dir, "plan.txt"), "--title", "T"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "line\n"); got != 80 {
		t.Errorf("default --lines: summary has %d plan lines, want 80", got)
	}
}

func TestCITFPlanLinesFlag(t *testing.T) {
	dir := t.TempDir()
	summary := filepath.Join(dir, "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)

	stubTFPlan(t, func(w io.Writer, _ []string) error {
		_, err := io.WriteString(w, "a\nb\nc\nd\n")
		return err
	})

	if err := execTFPlan(t, "--out", filepath.Join(dir, "plan.txt"), "--title", "T", "--lines", "2"); err != nil {
		t.Fatal(err)
	}
	want := "### T\n```\nc\nd\n```\n"
	if b, err := os.ReadFile(summary); err != nil || string(b) != want {
		t.Errorf("summary: got %q, %v\nwant %q", b, err, want)
	}
}

func TestCITFPlanFailurePropagatesAndSkipsSummary(t *testing.T) {
	dir := t.TempDir()
	tee := filepath.Join(dir, "plan.txt")
	summary := filepath.Join(dir, "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)

	planErr := errors.New("exit status 1")
	stubTFPlan(t, func(w io.Writer, _ []string) error {
		io.WriteString(w, "Error: invalid provider config\n")
		return planErr
	})

	err := execTFPlan(t, "--out", tee, "--title", "T")
	if err == nil || !errors.Is(err, planErr) {
		t.Fatalf("want wrapped plan error, got %v", err)
	}
	// The tee file keeps the partial output (tee semantics) …
	if b, _ := os.ReadFile(tee); !strings.Contains(string(b), "invalid provider config") {
		t.Errorf("tee file missing failure output: %q", b)
	}
	// … but no summary is written, matching the scripts' set -e behavior.
	if _, statErr := os.Stat(summary); !os.IsNotExist(statErr) {
		t.Errorf("summary written on failure (stat err %v)", statErr)
	}
}

func TestCITFPlanRetriesTransientAPIFlake(t *testing.T) {
	prev := tfPlanFlakeSettle
	tfPlanFlakeSettle = 0
	t.Cleanup(func() { tfPlanFlakeSettle = prev })

	dir := t.TempDir()
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(dir, "summary.md"))

	calls := 0
	stubTFPlan(t, func(w io.Writer, _ []string) error {
		calls++
		if calls == 1 {
			io.WriteString(w, `Error: Get "https://lke622766.api.us-ord.enterprise.linodelke.net:6443/api/v1/namespaces/kube-system/services/coredns": net/http: TLS handshake timeout`+"\n")
			return errors.New("exit status 1")
		}
		io.WriteString(w, "Plan: 1 to add, 0 to change, 0 to destroy.\n")
		return nil
	})

	if err := execTFPlan(t, "--out", filepath.Join(dir, "plan.txt"), "--title", "T"); err != nil {
		t.Fatalf("transient flake should be retried to success, got %v", err)
	}
	if calls != 2 {
		t.Errorf("want 2 plan attempts (flake + retry), got %d", calls)
	}
}

func TestCITFPlanDoesNotRetryGenuineError(t *testing.T) {
	prev := tfPlanFlakeSettle
	tfPlanFlakeSettle = 0
	t.Cleanup(func() { tfPlanFlakeSettle = prev })

	calls := 0
	stubTFPlan(t, func(w io.Writer, _ []string) error {
		calls++
		io.WriteString(w, "Error: Reference to undeclared resource\n")
		return errors.New("exit status 1")
	})

	if err := execTFPlan(t, "--out", filepath.Join(t.TempDir(), "plan.txt"), "--title", "T"); err == nil {
		t.Fatal("a genuine plan error must propagate")
	}
	if calls != 1 {
		t.Errorf("a non-flake error must NOT be retried; got %d attempts", calls)
	}
}

func TestCITFPlanSummarySkippedWhenEnvUnset(t *testing.T) {
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	stubTFPlan(t, func(w io.Writer, _ []string) error {
		_, err := io.WriteString(w, "ok\n")
		return err
	})
	if err := execTFPlan(t, "--out", filepath.Join(t.TempDir(), "plan.txt"), "--title", "T"); err != nil {
		t.Fatal(err)
	}
}

func TestCITFPlanRequiredFlags(t *testing.T) {
	stubTFPlan(t, func(io.Writer, []string) error {
		t.Fatal("terraform must not run when required flags are missing")
		return nil
	})
	if err := execTFPlan(t, "--title", "T"); err == nil || !strings.Contains(err.Error(), "out") {
		t.Errorf("missing --out: got %v", err)
	}
	if err := execTFPlan(t, "--out", "x"); err == nil || !strings.Contains(err.Error(), "title") {
		t.Errorf("missing --title: got %v", err)
	}
}

func TestCITFPlanTeeCreateError(t *testing.T) {
	stubTFPlan(t, func(io.Writer, []string) error {
		t.Fatal("terraform must not run when the tee file can't be created")
		return nil
	})
	err := execTFPlan(t, "--out", filepath.Join(t.TempDir(), "missing-dir", "plan.txt"), "--title", "T")
	if err == nil {
		t.Fatal("want error creating tee file")
	}
}

func TestTFPlanSummaryAppendOpenError(t *testing.T) {
	// Pointing the env var at a directory makes appendGHAFile's open fail; the
	// command must surface that rather than silently dropping the summary.
	t.Setenv("GITHUB_STEP_SUMMARY", t.TempDir())
	stubTFPlan(t, func(w io.Writer, _ []string) error { _, err := io.WriteString(w, "ok\n"); return err })
	err := execTFPlan(t, "--out", filepath.Join(t.TempDir(), "plan.txt"), "--title", "T")
	if err == nil {
		t.Fatal("want error appending to a directory")
	}
}

// TestTFPlanRunFnRealExec covers the default seam: a fake `terraform` on PATH
// proves the argv shape and that stdout+stderr are combined into one writer.
func TestTFPlanRunFnRealExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-binary PATH trick needs a POSIX shell")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "terraform")
	script := "#!/bin/sh\necho \"argv: $@\"\necho \"err line\" >&2\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var buf bytes.Buffer
	if err := tfPlanRunFn(&buf, []string{"-var", "x=1"}); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "argv: plan -no-color -var x=1") {
		t.Errorf("argv: got %q", got)
	}
	if !strings.Contains(got, "err line") {
		t.Errorf("stderr not combined: got %q", got)
	}
}
