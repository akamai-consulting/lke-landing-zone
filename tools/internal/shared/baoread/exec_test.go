package baoread

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// withBaoExec swaps the ExecFn seam for the test's duration.
func withBaoExec(t *testing.T, fn func(pod, token, stdin string, args ...string) (string, string, error)) {
	t.Helper()
	orig := ExecFn
	ExecFn = fn
	t.Cleanup(func() { ExecFn = orig })
}

// withBaoSleep makes poll waits instantaneous while counting them.
func withBaoSleep(t *testing.T) *int {
	t.Helper()
	orig := Sleep
	n := new(int)
	Sleep = func(time.Duration) { *n++ }
	t.Cleanup(func() { Sleep = orig })
	return n
}

// withBaoExecRaw swaps the raw (pre-retry) exec seam so the retry wrapper
// itself can be exercised; the live ExecFn (= execResilient) stays wired.
func withBaoExecRaw(t *testing.T, fn func(pod, token, stdin string, args ...string) (string, string, error)) {
	t.Helper()
	orig := ExecRaw
	ExecRaw = fn
	t.Cleanup(func() { ExecRaw = orig })
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
	out, _, err := execResilient("platform-openbao-0", "", "", "operator", "init")
	if err != nil {
		t.Fatalf("execResilient returned err after transient retries: %v", err)
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
	if _, _, err := execResilient("platform-openbao-0", "", ""); err == nil {
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
	if _, _, err := execResilient("platform-openbao-0", "", ""); err == nil {
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
		st, ok := ParsePodStatus(c.in)
		if ok != c.ok || st.Initialized != c.initialized || st.Sealed != c.sealed {
			t.Errorf("ParsePodStatus(%q) = (%+v, %v), want init=%v sealed=%v ok=%v",
				c.in, st, ok, c.initialized, c.sealed, c.ok)
		}
	}
}

func TestAggregateBaoStatus(t *testing.T) {
	cases := []struct {
		states      []PodStatus
		initialized bool
		sealed      bool
	}{
		// Healthy steady state.
		{[]PodStatus{{true, false}, {true, false}, {true, false}}, true, false},
		// Partial seal MUST read as sealed (quorum risk).
		{[]PodStatus{{true, false}, {true, true}, {true, false}}, true, true},
		// Fresh cluster.
		{[]PodStatus{{false, true}, {false, true}, {false, true}}, false, true},
		// One pod knows it's initialized → cluster-wide flag.
		{[]PodStatus{{false, true}, {true, true}, {false, true}}, true, true},
	}
	for i, c := range cases {
		gotInit, gotSealed := AggregateStatus(c.states)
		if gotInit != c.initialized || gotSealed != c.sealed {
			t.Errorf("case %d: aggregate = (%v, %v), want (%v, %v)", i, gotInit, gotSealed, c.initialized, c.sealed)
		}
	}
}

func TestRecoveryKeysFromEnv(t *testing.T) {
	t.Setenv("RECOVERY_K1", "k1")
	t.Setenv("RECOVERY_K2", "k2")
	t.Setenv("RECOVERY_K3", "k3")
	keys, err := RecoveryKeysFromEnv()
	if err != nil || len(keys) != 3 || keys[2] != "k3" {
		t.Fatalf("RecoveryKeysFromEnv = (%v, %v), want 3 keys", keys, err)
	}
	t.Setenv("RECOVERY_K2", "")
	if _, err := RecoveryKeysFromEnv(); err == nil || !strings.Contains(err.Error(), "RECOVERY_K2") {
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
	ok := WaitForState("platform-openbao-1", 300*time.Second, 5*time.Second, func(st PodStatus) bool {
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
	if WaitForState("platform-openbao-2", 20*time.Second, 5*time.Second, func(st PodStatus) bool { return st.Initialized }) {
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
	if err := WaitForAutoUnseal(180*time.Second, 300*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForAutoUnsealLeaderTimeout(t *testing.T) {
	withBaoSleep(t)
	withExecOutput(t, func(string, ...string) ([]byte, error) { return []byte(""), nil })
	reads := 0
	withBaoExec(t, func(pod, _, _ string, args ...string) (string, string, error) {
		// Leader never auto-unseals (e.g. missing/wrong static seal key). The read
		// cap keeps that from being literally forever: WaitForState spends its
		// budget by accumulating the interval, so a collapsed interval would poll
		// without end — the cap turns that into the failed assertion below.
		reads++
		if reads > 50 {
			return `{"initialized":true,"sealed":false}`, "", nil
		}
		return `{"initialized":true,"sealed":true}`, "", errors.New("exit status 2")
	})
	err := WaitForAutoUnseal(10*time.Second, 10*time.Second)
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
	followerReads := 0
	withBaoExec(t, func(pod, _, _ string, args ...string) (string, string, error) {
		if pod == "platform-openbao-0" {
			return `{"initialized":true,"sealed":false}`, "", nil
		}
		followerReads++ // see the read cap in TestWaitForAutoUnsealLeaderTimeout
		if followerReads > 50 {
			return `{"initialized":true,"sealed":false}`, "", nil
		}
		return `{"initialized":false,"sealed":true}`, "", errors.New("exit status 2")
	})
	err := WaitForAutoUnseal(10*time.Second, 10*time.Second)
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

// ── "no container to exec into" is not a transport failure ───────────────────

// kubeletNoContainer is the kubelet's verbatim answer when a pod has no such
// container. Note the PREFIX: `unable to upgrade connection` is also the SPDY
// transport marker, which is exactly how this came to be retried as a blip.
const kubeletNoContainer = `error: Internal error occurred: unable to upgrade connection: container not found ("openbao")`

// TestPodStateExecErrIsNotTransient is the classification gate. On
// akamai/gsap-apl one OpenBao replica sat in CrashLoopBackOff for eight days and
// every exec against it spent the full konnectivity budget before reporting a
// transport fault — for a pod that had no container at all.
func TestPodStateExecErrIsNotTransient(t *testing.T) {
	if !isPodStateExecErr(kubeletNoContainer) {
		t.Errorf("isPodStateExecErr(%q) = false — the kubelet answered; this is pod state", kubeletNoContainer)
	}
	if isTransientExecErr(kubeletNoContainer) {
		t.Errorf("isTransientExecErr(%q) = true — its transport-marker PREFIX must not win over the kubelet's answer", kubeletNoContainer)
	}
	// The transport cases it is worded like must stay transient, including the
	// `unable to upgrade connection` family this now has to disambiguate.
	for _, s := range []string{
		"unable to upgrade connection: pod does not exist",
		"error dialing backend: remote error",
		"No agent available",
	} {
		if isPodStateExecErr(s) {
			t.Errorf("isPodStateExecErr(%q) = true, want false", s)
		}
		if !isTransientExecErr(s) {
			t.Errorf("isTransientExecErr(%q) = false — narrowing must not cost a real transport case", s)
		}
	}
}

// TestBaoExecResilientPodStateUsesShortBudget pins the budget SPLIT. The cost of
// the old behaviour was not one wasted wait but the konnectivity budget spent per
// exec call, and the seal/token lifecycle makes several against each pod.
func TestBaoExecResilientPodStateUsesShortBudget(t *testing.T) {
	withBaoSleep(t)
	calls := 0
	withBaoExecRaw(t, func(_, _, _ string, _ ...string) (string, string, error) {
		calls++
		return "", kubeletNoContainer, errors.New("exit 1")
	})
	var err error
	out := captureStderr(t, func() { _, _, err = execResilient("platform-openbao-2", "", "") })
	if err == nil {
		t.Fatal("expected the error to surface once the pod-state budget is spent")
	}
	if calls != podStateRetries {
		t.Errorf("raw exec called %d times, want podStateRetries=%d (not baoExecRetries=%d)",
			calls, podStateRetries, baoExecRetries)
	}
	if podStateRetries >= baoExecRetries {
		t.Errorf("podStateRetries=%d must be well under baoExecRetries=%d — the node already answered",
			podStateRetries, baoExecRetries)
	}
	// The diagnostic has to name the pod and say this is pod state. The kubelet's
	// own wording sends the reader to konnectivity; that misdirection is the bug.
	if !strings.Contains(out, "platform-openbao-2") || !strings.Contains(out, "POD STATE") {
		t.Errorf("give-up diagnostic must name the pod and the class, got:\n%s", out)
	}
	if !strings.Contains(out, "describe pod") {
		t.Errorf("give-up diagnostic must point at the pod, got:\n%s", out)
	}
}

// TestBaoExecResilientPodStateStillRetriesBriefly: a pod that has just reached
// Running can be a second from having its container, so this must not fail on the
// first answer. The short budget is a budget, not a hard stop.
func TestBaoExecResilientPodStateStillRetriesBriefly(t *testing.T) {
	withBaoSleep(t)
	calls := 0
	withBaoExecRaw(t, func(_, _, _ string, _ ...string) (string, string, error) {
		calls++
		if calls < 3 {
			return "", kubeletNoContainer, errors.New("exit 1")
		}
		return `{"sealed":false}`, "", nil
	})
	var err error
	captureStderr(t, func() { _, _, err = execResilient("platform-openbao-2", "", "") })
	if err != nil {
		t.Fatalf("a container that appears on attempt 3 must succeed, got: %v", err)
	}
	if calls != 3 {
		t.Errorf("raw exec called %d times, want 3 (2 pod-state + 1 success)", calls)
	}
}
