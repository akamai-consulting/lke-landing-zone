package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// withBaoExec swaps the baoExecFn seam for the test's duration.
func withBaoExec(t *testing.T, fn func(pod, token, stdin string, args ...string) (string, string, error)) {
	t.Helper()
	orig := baoExecFn
	baoExecFn = fn
	t.Cleanup(func() { baoExecFn = orig })
}

// withBaoSleep makes poll waits instantaneous while counting them.
func withBaoSleep(t *testing.T) *int {
	t.Helper()
	orig := baoSleep
	n := new(int)
	baoSleep = func(time.Duration) { *n++ }
	t.Cleanup(func() { baoSleep = orig })
	return n
}

// withBaoExecRaw swaps the raw (pre-retry) exec seam so the retry wrapper
// itself can be exercised; the live baoExecFn (= baoExecResilient) stays wired.
func withBaoExecRaw(t *testing.T, fn func(pod, token, stdin string, args ...string) (string, string, error)) {
	t.Helper()
	orig := baoExecRawFn
	baoExecRawFn = fn
	t.Cleanup(func() { baoExecRawFn = orig })
}

func TestIsTransientExecErr(t *testing.T) {
	transient := []string{
		`Internal error occurred: error sending request: Post "https://10.0.0.5:10250/exec/...": No agent available`,
		"error dialing backend: remote error",
		"unable to upgrade connection: pod does not exist",
		"net/http: TLS handshake timeout",
	}
	for _, s := range transient {
		if !isTransientExecErr(s) {
			t.Errorf("isTransientExecErr(%q) = false, want true", s)
		}
	}
	// A genuine bao error (sealed-pod status, already-initialized) must NOT
	// be treated as transient — retrying it would mask the real result.
	notTransient := []string{
		"",
		"Error initializing: Vault is already initialized",
		"Error unsealing: Vault is sealed",
		`{"sealed":true,"initialized":false}`,
	}
	for _, s := range notTransient {
		if isTransientExecErr(s) {
			t.Errorf("isTransientExecErr(%q) = true, want false", s)
		}
	}
}

func TestBaoExecResilientRetriesTransient(t *testing.T) {
	withBaoSleep(t)
	calls := 0
	withBaoExecRaw(t, func(_, _, _ string, _ ...string) (string, string, error) {
		calls++
		if calls < 3 {
			return "", "Internal error occurred: ... No agent available", errors.New("exit 1")
		}
		return `{"root_token":"s.x"}`, "", nil
	})
	out, _, err := baoExecResilient("platform-openbao-0", "", "", "operator", "init")
	if err != nil {
		t.Fatalf("baoExecResilient returned err after transient retries: %v", err)
	}
	if calls != 3 {
		t.Errorf("raw exec called %d times, want 3 (2 transient + 1 success)", calls)
	}
	if out != `{"root_token":"s.x"}` {
		t.Errorf("stdout = %q, want the success payload", out)
	}
}

func TestBaoExecResilientNoRetryOnRealError(t *testing.T) {
	n := withBaoSleep(t)
	calls := 0
	withBaoExecRaw(t, func(_, _, _ string, _ ...string) (string, string, error) {
		calls++
		return "", "Error: Vault is already initialized", errors.New("exit 2")
	})
	if _, _, err := baoExecResilient("platform-openbao-0", "", ""); err == nil {
		t.Fatal("expected the genuine bao error to propagate")
	}
	if calls != 1 {
		t.Errorf("raw exec called %d times, want 1 (no retry on a real error)", calls)
	}
	if *n != 0 {
		t.Errorf("slept %d times, want 0 (no backoff on a real error)", *n)
	}
}

func TestBaoExecResilientGivesUpAfterBudget(t *testing.T) {
	withBaoSleep(t)
	calls := 0
	withBaoExecRaw(t, func(_, _, _ string, _ ...string) (string, string, error) {
		calls++
		return "", "No agent available", errors.New("exit 1")
	})
	if _, _, err := baoExecResilient("platform-openbao-0", "", ""); err == nil {
		t.Fatal("expected the error to surface once the retry budget is spent")
	}
	if calls != baoExecRetries {
		t.Errorf("raw exec called %d times, want baoExecRetries=%d", calls, baoExecRetries)
	}
}

func TestParseBaoPodStatus(t *testing.T) {
	cases := []struct {
		in          string
		ok          bool
		initialized bool
		sealed      bool
	}{
		{`{"initialized":true,"sealed":false}`, true, true, false},
		{`{"initialized":true,"sealed":true,"t":3,"n":5}`, true, true, true},
		{`{"initialized":false,"sealed":true}`, true, false, true},
		// No JSON at all (pod unreachable) → uninitialized+sealed default.
		{"", false, false, true},
		{"error: unable to connect", false, false, true},
	}
	for _, c := range cases {
		st, ok := parseBaoPodStatus(c.in)
		if ok != c.ok || st.Initialized != c.initialized || st.Sealed != c.sealed {
			t.Errorf("parseBaoPodStatus(%q) = (%+v, %v), want init=%v sealed=%v ok=%v",
				c.in, st, ok, c.initialized, c.sealed, c.ok)
		}
	}
}

func TestAggregateBaoStatus(t *testing.T) {
	cases := []struct {
		states      []baoPodStatus
		initialized bool
		sealed      bool
	}{
		// Healthy steady state.
		{[]baoPodStatus{{true, false}, {true, false}, {true, false}}, true, false},
		// Partial seal MUST read as sealed (quorum risk).
		{[]baoPodStatus{{true, false}, {true, true}, {true, false}}, true, true},
		// Fresh cluster.
		{[]baoPodStatus{{false, true}, {false, true}, {false, true}}, false, true},
		// One pod knows it's initialized → cluster-wide flag.
		{[]baoPodStatus{{false, true}, {true, true}, {false, true}}, true, true},
	}
	for i, c := range cases {
		gotInit, gotSealed := aggregateBaoStatus(c.states)
		if gotInit != c.initialized || gotSealed != c.sealed {
			t.Errorf("case %d: aggregate = (%v, %v), want (%v, %v)", i, gotInit, gotSealed, c.initialized, c.sealed)
		}
	}
}

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
	if err := appendGHAFile("GITHUB_OUTPUT", "k=v"); err != nil {
		t.Errorf("appendGHAFile with unset env = %v, want nil", err)
	}
}

func TestAppendGHAFileAppends(t *testing.T) {
	f := filepath.Join(t.TempDir(), "env")
	t.Setenv("GITHUB_ENV", f)
	if err := appendGHAFile("GITHUB_ENV", "A=1"); err != nil {
		t.Fatal(err)
	}
	if err := appendGHAFile("GITHUB_ENV", "B=2", "C=3"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f)
	if got := string(b); got != "A=1\nB=2\nC=3\n" {
		t.Errorf("GITHUB_ENV = %q, want three appended lines", got)
	}
}

func TestRecoveryKeysFromEnv(t *testing.T) {
	t.Setenv("RECOVERY_K1", "k1")
	t.Setenv("RECOVERY_K2", "k2")
	t.Setenv("RECOVERY_K3", "k3")
	keys, err := recoveryKeysFromEnv()
	if err != nil || len(keys) != 3 || keys[2] != "k3" {
		t.Fatalf("recoveryKeysFromEnv = (%v, %v), want 3 keys", keys, err)
	}
	t.Setenv("RECOVERY_K2", "")
	if _, err := recoveryKeysFromEnv(); err == nil || !strings.Contains(err.Error(), "RECOVERY_K2") {
		t.Errorf("missing RECOVERY_K2 → err = %v, want named error", err)
	}
}

func TestWaitForBaoState(t *testing.T) {
	sleeps := withBaoSleep(t)
	probes := 0
	withBaoExec(t, func(pod, _, _ string, _ ...string) (string, string, error) {
		probes++
		if probes >= 3 {
			return `{"initialized":true,"sealed":true}`, "", errors.New("exit status 2")
		}
		return "", "", errors.New("not up yet")
	})
	ok := waitForBaoState("platform-openbao-1", 300*time.Second, 5*time.Second, func(st baoPodStatus) bool {
		return st.Initialized
	})
	if !ok || probes != 3 || *sleeps != 2 {
		t.Errorf("ok=%v probes=%d sleeps=%d, want success on 3rd probe after 2 sleeps", ok, probes, *sleeps)
	}
}

func TestWaitForBaoStateTimesOut(t *testing.T) {
	sleeps := withBaoSleep(t)
	withBaoExec(t, func(string, string, string, ...string) (string, string, error) {
		return `{"initialized":false,"sealed":true}`, "", errors.New("exit status 2")
	})
	if waitForBaoState("platform-openbao-2", 20*time.Second, 5*time.Second, func(st baoPodStatus) bool { return st.Initialized }) {
		t.Fatal("want timeout, got success")
	}
	// 20s budget / 5s interval → probes at 0,5,10,15,20s then give up: 4 sleeps
	// (like the script, the boundary probe at elapsed==budget still happens).
	if *sleeps != 4 {
		t.Errorf("sleeps = %d, want 4 within a 20s budget", *sleeps)
	}
}

func TestWaitForAutoUnsealHappyPath(t *testing.T) {
	withBaoSleep(t)
	followerProbes := map[string]int{}
	withBaoExec(t, func(pod, _, _ string, args ...string) (string, string, error) {
		if pod == "platform-openbao-0" {
			return `{"initialized":true,"sealed":false}`, "", nil
		}
		// Followers retry_join then auto-unseal on their second probe (boot race):
		// initialized flips true and sealed flips false together.
		followerProbes[pod]++
		up := followerProbes[pod] >= 2
		return fmt.Sprintf(`{"initialized":%t,"sealed":%t}`, up, !up), "", errors.New("exit status 2")
	})
	if err := waitForAutoUnseal(180*time.Second, 300*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForAutoUnsealLeaderTimeout(t *testing.T) {
	withBaoSleep(t)
	withExecOutput(t, func(string, ...string) ([]byte, error) { return []byte(""), nil })
	withBaoExec(t, func(pod, _, _ string, args ...string) (string, string, error) {
		// Leader never auto-unseals (e.g. missing/wrong static seal key).
		return `{"initialized":true,"sealed":true}`, "", errors.New("exit status 2")
	})
	err := waitForAutoUnseal(10*time.Second, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "leader") {
		t.Errorf("err = %v, want leader timeout", err)
	}
}

func TestWaitForAutoUnsealFollowerTimeoutDumpsLogs(t *testing.T) {
	withBaoSleep(t)
	logsFetched := false
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name == "kubectl" && len(args) > 2 && args[2] == "logs" {
			logsFetched = true
		}
		return []byte("retry_join: failed to get raft challenge"), nil
	})
	withBaoExec(t, func(pod, _, _ string, args ...string) (string, string, error) {
		if pod == "platform-openbao-0" {
			return `{"initialized":true,"sealed":false}`, "", nil
		}
		return `{"initialized":false,"sealed":true}`, "", errors.New("exit status 2")
	})
	err := waitForAutoUnseal(10*time.Second, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "platform-openbao-1") {
		t.Errorf("err = %v, want follower-1 timeout", err)
	}
	if !logsFetched {
		t.Error("follower timeout did not fetch container logs for diagnostics")
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
