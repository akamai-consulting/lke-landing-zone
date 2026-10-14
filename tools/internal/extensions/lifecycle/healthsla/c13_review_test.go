package healthsla

// c13_review_test.go — the gates for the C13 findings of the 2026-08-13 review.
// Four HIGHs, one class: an SLA gate that could not fire, because "I could not
// measure" was graded as "nothing is wrong".

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// ── `bao status` exits non-zero PRECISELY when sealed ────────────────────────

// TestBaoStatusReadsASealedPod.
//
// `bao status` exits 2 when the pod is sealed or uninitialised — and still prints
// valid JSON. Returning early on the exec error therefore reported every SEALED
// pod as "seal state UNKNOWN", so the `sealed` counter this readiness summary
// publishes could never increment: the one state it exists to surface was the one
// state it could not see. baoread.ParsePodStatus's doc states the rule verbatim.
//
// The pre-existing test's sealed case returned (json, nil) — a shape the real
// exec never produces — which is why the defect survived it.
func TestBaoStatusReadsASealedPod(t *testing.T) {
	// ensureDeps first, then override ONE field. A bare `td = Deps{...}` leaves
	// Summary and Exec nil in a package-level global for whatever runs next, and
	// is only survivable because every other test happens to call ensureDeps.
	ensureDeps(t)
	td.BaoExec = func(string, string, string, ...string) (string, string, error) {
		// What the real exec returns for a sealed pod: good JSON, exit 2.
		return `{"initialized":true,"sealed":true}`, "", errors.New("command terminated with exit code 2")
	}
	st, ok := baoStatus(td, "platform-openbao-0")
	if !ok {
		t.Fatal("a sealed pod prints valid JSON and exits 2 — reading that as `could not determine` means " +
			"the sealed counter can never increment")
	}
	if !st.Sealed {
		t.Errorf("status = %+v, want Sealed", st)
	}
}

// TestBaoStatusStillReportsUnknownWithNoJSON pins the exclusion the fix must not
// erase: no JSON at all means the exec did not reach a running bao, which is a
// DIFFERENT problem from a sealed pod and must not be counted as one — that sends
// the operator to the unseal key, the static seal key and Raft storage, three
// places that are all fine.
func TestBaoStatusStillReportsUnknownWithNoJSON(t *testing.T) {
	ensureDeps(t)
	td.BaoExec = func(string, string, string, ...string) (string, string, error) {
		return "", "error dialing backend: No agent available", errors.New("exit 1")
	}
	if _, ok := baoStatus(td, "platform-openbao-0"); ok {
		t.Error("an exec that produced no JSON is `could not determine`, not a seal state")
	}
}

// ── the lists that reported clean having read nothing ────────────────────────

// TestCertManagerCheckFailsOnAnUnreadableList. A failed apiserver read yields
// zero items, the loop body never runs, notReady stays 0, and this printed "All
// cert-manager Certificates Ready" — the false all-clear the ExternalSecrets
// branch twenty lines below already guards against.
func TestCertManagerCheckFailsOnAnUnreadableList(t *testing.T) {
	testDeps(t)
	sum := withSummaryFile(t)
	prevRetries, prevDelay := kubectlprobe.Retries, kubectlprobe.Delay
	kubectlprobe.Retries, kubectlprobe.Delay = 1, 0
	t.Cleanup(func() { kubectlprobe.Retries, kubectlprobe.Delay = prevRetries, prevDelay })
	kubectlprobe.Exec = func(string, ...string) ([]byte, error) {
		return nil, errors.New("the connection to the server was refused")
	}

	if err := RunCertManager(td); err == nil {
		t.Fatal("a Certificate list that could not be read must not report every Certificate Ready")
	}
	if body := sum(); strings.Contains(body, "All Certificates Ready") {
		t.Errorf("the summary claims every Certificate is Ready having read none:\n%s", body)
	}
}

// TestLkeAdminSLAFailsOnAnUnreadableSecretList. A read that fails AFTER
// Reachable() passed yields zero items, MaxTime reports nothing found, and the
// function warned and returned nil. The hard 90-day gate over the most privileged
// credential on the cluster was one RBAC change from permanently green — reporting
// "No lke-admin-token Secret found", a claim about the cluster made without
// having read it.
func TestLkeAdminSLAFailsOnAnUnreadableSecretList(t *testing.T) {
	testDeps(t)
	withSummaryFile(t)
	prevRetries, prevDelay := kubectlprobe.Retries, kubectlprobe.Delay
	kubectlprobe.Retries, kubectlprobe.Delay = 1, 0
	t.Cleanup(func() { kubectlprobe.Retries, kubectlprobe.Delay = prevRetries, prevDelay })
	kubectlprobe.Exec = func(string, ...string) ([]byte, error) {
		return nil, errors.New(`secrets is forbidden: User cannot list resource "secrets"`)
	}

	err := RunLKEAdminRotation(td, 60, 90)
	if err == nil {
		t.Fatal("a Secret list that could not be read must not pass the lke-admin rotation SLA")
	}
	if !strings.Contains(err.Error(), "could not be measured") {
		t.Errorf("the error must say the SLA was not measured, got: %v", err)
	}
}

// ── the slot mismatch ────────────────────────────────────────────────────────

// TestTheProductionBaoExecForwardsIntoTheRightSlots. Deps.BaoExec was declared
// `(pod, addr, token string, ...)` and forwarded positionally into
// `baoread.ExecFn(pod, token, stdin string, ...)` — so `addr` landed in the TOKEN
// slot and `token` in the STDIN slot. Inert while both are "", but the first
// authenticated caller would have piped its token to the child's stdin and run
// the command UNAUTHENTICATED, with the token in a place nothing redacts.
//
// STUBS baoread.ExecFn AND DRIVES THE REAL HealthSLADeps() CLOSURE. A first cut
// asserted on a stub it had just written into td: parameter NAMES are not part of
// a Go func type, so that test passed with the production forwarding reverted to
// the broken order — it exercised nothing but itself. The only thing that can
// fail here is the wiring under review.
func TestTheProductionBaoExecForwardsIntoTheRightSlots(t *testing.T) {
	prev := baoread.ExecFn
	t.Cleanup(func() { baoread.ExecFn = prev })
	var gotPod, gotToken, gotStdin string
	var gotArgs []string
	baoread.ExecFn = func(pod, token, stdin string, args ...string) (string, string, error) {
		gotPod, gotToken, gotStdin, gotArgs = pod, token, stdin, args
		return "", "", nil
	}

	_, _, _ = HealthSLADeps().BaoExec("platform-openbao-0", "s.a-real-token", "stdin-payload", "status", "-format=json")

	if gotPod != "platform-openbao-0" {
		t.Errorf("pod slot = %q", gotPod)
	}
	if gotToken != "s.a-real-token" {
		t.Errorf("token slot = %q — a token in the wrong slot runs the command UNAUTHENTICATED and "+
			"lands the secret somewhere nothing redacts", gotToken)
	}
	if gotStdin != "stdin-payload" {
		t.Errorf("stdin slot = %q", gotStdin)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "status" {
		t.Errorf("args = %v, want the variadic tail forwarded unchanged", gotArgs)
	}
}

// withSummaryFile is setSummary (sla_test.go) plus a reader for what was
// written. It calls setSummary rather than re-Setenv-ing the file, because the
// first cut duplicated it and omitted REGION — so the new tests rendered a
// different region label from every other test in the package and asserted on it.
func withSummaryFile(t *testing.T) func() string {
	t.Helper()
	setSummary(t)
	p := os.Getenv("GITHUB_STEP_SUMMARY")
	return func() string {
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
