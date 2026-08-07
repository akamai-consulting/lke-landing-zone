package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghaout"
	"github.com/spf13/cobra"
)

// ci_bao_status_test.go — the tests that came back with the command.
//
// These moved to internal/baoread with the exec layer and had to come back: they
// are about the cobra lane and the GITHUB_OUTPUT append, neither of which is
// part of the exec layer. Filename-as-subject again — they lived in
// ci_openbao_test.go, so they travelled with a file whose subject was not theirs.

func TestRunCIBaoStatusWritesOutputs(t *testing.T) {
	out := filepath.Join(t.TempDir(), "output")
	t.Setenv("GITHUB_OUTPUT", out)
	withBaoExec(t, func(pod, _, _ string, args ...string) (string, string, error) {
		if args[0] != "status" {
			t.Errorf("unexpected bao args %v", args)
		}
		switch pod {
		case "platform-openbao-0":
			return `{"initialized":true,"sealed":false}`, "", nil
		case "platform-openbao-1":
			// Sealed pods exit non-zero but still print JSON.
			return `{"initialized":true,"sealed":true}`, "", errors.New("exit status 2")
		default:
			// Unreachable pod: no JSON at all.
			return "", "connection refused", errors.New("exit status 1")
		}
	})
	if err := runCIBaoStatus(); err != nil {
		t.Fatalf("runCIBaoStatus: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read GITHUB_OUTPUT: %v", err)
	}
	if got := string(b); got != "initialized=true\nsealed=true\n" {
		t.Errorf("GITHUB_OUTPUT = %q, want initialized=true + sealed=true", got)
	}
}

func TestAppendGHAFileNoEnvIsNoop(t *testing.T) {
	t.Setenv("GITHUB_OUTPUT", "")
	if err := ghaout.Append("GITHUB_OUTPUT", "k=v"); err != nil {
		t.Errorf("ghaout.Append with unset env = %v, want nil", err)
	}
}

func TestAppendGHAFileAppends(t *testing.T) {
	f := filepath.Join(t.TempDir(), "env")
	t.Setenv("GITHUB_ENV", f)
	if err := ghaout.Append("GITHUB_ENV", "A=1"); err != nil {
		t.Fatal(err)
	}
	if err := ghaout.Append("GITHUB_ENV", "B=2", "C=3"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f)
	if got := string(b); got != "A=1\nB=2\nC=3\n" {
		t.Errorf("GITHUB_ENV = %q, want three appended lines", got)
	}
}

// TestCIBaoCommandWiring executes every `llz ci bao-*` cobra command end to
// end (flag parsing → RunE) under --dry-run with the exec/gh seams stubbed,
// pinning the Use strings and required-flag errors the workflows depend on.
func TestCIBaoCommandWiring(t *testing.T) {
	t.Setenv("RECOVERY_K1", "k1")
	t.Setenv("RECOVERY_K2", "k2")
	t.Setenv("RECOVERY_K3", "k3")
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	t.Setenv("GITHUB_OUTPUT", "")
	withBaoExec(t, func(string, string, string, ...string) (string, string, error) {
		return `{"initialized":true,"sealed":false}`, "", nil
	})
	withGHSetSecret(t, nil)
	origOpts := gopts
	gopts = globalOpts{dryRun: true}
	t.Cleanup(func() { gopts = origOpts })

	cases := []struct {
		cmd  func() *cobra.Command
		use  string
		args []string
	}{
		{ciBaoStatusCmd, "bao-status", nil},
		{ciBaoInitCmd, "bao-init", []string{"--region", "primary"}},
		{ciBaoRegenRootCmd, "bao-regen-root", []string{"--region", "primary"}},
		{ciBaoConfigureCmd, "bao-configure", []string{"--region", "primary"}},
	}
	for _, c := range cases {
		cmd := c.cmd()
		if cmd.Use != c.use {
			t.Errorf("Use = %q, want %q", cmd.Use, c.use)
		}
		cmd.SetArgs(c.args)
		cmd.SilenceUsage = true
		if err := cmd.Execute(); err != nil {
			t.Errorf("%s %v: %v", c.use, c.args, err)
		}
	}

	// The region-taking commands refuse to run without --region.
	for _, mk := range []func() *cobra.Command{ciBaoInitCmd, ciBaoRegenRootCmd, ciBaoConfigureCmd} {
		cmd := mk()
		cmd.SetArgs(nil)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--region") {
			t.Errorf("%s without --region: err = %v, want required-flag error", cmd.Use, err)
		}
	}
}
