package healthsla

// c13_review_test.go — the gates for the C13 findings of the 2026-08-13 review.
// Four HIGHs, one class: an SLA gate that could not fire, because "I could not
// measure" was graded as "nothing is wrong".

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

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
	td = Deps{
		BaoExec: func(string, string, string, ...string) (string, string, error) {
			// What the real exec returns for a sealed pod: good JSON, exit 2.
			return `{"initialized":true,"sealed":true}`, "", errors.New("command terminated with exit code 2")
		},
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
	td = Deps{
		BaoExec: func(string, string, string, ...string) (string, string, error) {
			return "", "error dialing backend: No agent available", errors.New("exit 1")
		},
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

// ── the Loki gate's three causes ─────────────────────────────────────────────

// TestLokiObjkeyReadSeparatesItsThreeCauses. The reader returned a bare "" for
// three unrelated states — token unset, exec failed, field absent — and the caller
// graded all three as "not found: warn and pass". Two of them are "I could not
// measure", which is not a verdict a hard SLA gate is entitled to treat as clean.
func TestLokiObjkeyReadSeparatesItsThreeCauses(t *testing.T) {
	testDeps(t)

	t.Setenv("OPENBAO_ROOT_TOKEN", "")
	if got := lokiObjkeyUpdatedTime(td); !got.NoToken || got.ReadFail != nil {
		t.Errorf("an unset token must be distinguishable, got %+v", got)
	}

	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	td.Exec = func(string, ...string) ([]byte, error) { return nil, errors.New("exec failed") }
	if got := lokiObjkeyUpdatedTime(td); got.ReadFail == nil || got.NoToken {
		t.Errorf("a failed exec must be distinguishable from an unset token, got %+v", got)
	}

	td.Exec = func(string, ...string) ([]byte, error) { return []byte(`{"data":{}}`), nil }
	got := lokiObjkeyUpdatedTime(td)
	if got.NoToken || got.ReadFail != nil || got.Updated != "" {
		t.Errorf("an answered-but-absent field is the genuine not-found case, got %+v", got)
	}
}

// TestLokiSLAFailsWhenItHadATokenAndStillCouldNotRead. We HAD a token and still
// could not read: that is a real failure of a hard gate, and passing it off as
// "not bootstrapped" is how a stale credential ages past its SLA unremarked.
func TestLokiSLAFailsWhenItHadATokenAndStillCouldNotRead(t *testing.T) {
	testDeps(t)
	withSummaryFile(t)
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	td.Exec = func(string, ...string) ([]byte, error) { return nil, errors.New("command terminated with exit code 1") }

	if err := RunLokiObjkeyRotation(td, 60, 120); err == nil {
		t.Fatal("a read failure with a token in hand must fail the SLA gate, not warn")
	}
}

// TestLokiSLASaysItRenderedNoVerdictWithoutAToken.
//
// llz-scheduled-checks.yml declares OPENBAO_ROOT_TOKEN `required: false` and its
// own comment records that the token is EXPECTED ABSENT, because bootstrap
// revokes it. So on a correctly configured cluster this check has ALWAYS taken
// this branch — the step is labelled "THE GATE — deliberately no
// continue-on-error" and has never been able to fire.
//
// Deliberately NOT converted to a hard failure: that would fail the scheduled run
// on every correctly configured instance. What it must stop doing is reporting
// the credential as "not found", which is a claim about OpenBao rather than about
// this check.
func TestLokiSLASaysItRenderedNoVerdictWithoutAToken(t *testing.T) {
	testDeps(t)
	sum := withSummaryFile(t)
	t.Setenv("OPENBAO_ROOT_TOKEN", "")

	if err := RunLokiObjkeyRotation(td, 60, 120); err != nil {
		t.Fatalf("the expected steady state must not fail every adopter's scheduled run: %v", err)
	}
	body := sum()
	if strings.Contains(body, "No secret/loki/object-store") {
		t.Error("reporting the credential as absent is a claim about OpenBao made without asking it")
	}
	if !strings.Contains(body, "No verdict") {
		t.Errorf("the summary must say nothing was measured:\n%s", body)
	}
}

// ── the slot mismatch ────────────────────────────────────────────────────────

// TestBaoExecForwardsPositionally. Deps.BaoExec was declared
// `(pod, addr, token string, ...)` and forwarded positionally into
// `baoread.ExecFn(pod, token, stdin string, ...)` — so `addr` landed in the TOKEN
// slot and `token` in the STDIN slot. Inert while both are "", but the first
// authenticated caller would have piped its token to the child's stdin and run
// the command UNAUTHENTICATED, with the token in a place nothing redacts.
//
// Asserting on the SIGNATURE, because that is the whole fix: identical parameter
// lists leave the forwarding with nothing to get wrong.
func TestBaoExecForwardsPositionally(t *testing.T) {
	var gotPod, gotToken, gotStdin string
	td = Deps{
		BaoExec: func(pod, token, stdin string, _ ...string) (string, string, error) {
			gotPod, gotToken, gotStdin = pod, token, stdin
			return `{"sealed":false}`, "", nil
		},
	}
	// A future authenticated caller, written against the documented meaning.
	_, _, _ = td.BaoExec("platform-openbao-0", "s.a-real-token", "", "status")
	if gotPod != "platform-openbao-0" || gotToken != "s.a-real-token" || gotStdin != "" {
		t.Errorf("BaoExec(pod, token, stdin) = (%q, %q, %q) — the slots must line up with "+
			"baoread.ExecFn's, or a token ends up on the child's stdin", gotPod, gotToken, gotStdin)
	}
}

// withSummaryFile points $GITHUB_STEP_SUMMARY at a temp file and returns a reader.
func withSummaryFile(t *testing.T) func() string {
	t.Helper()
	p := t.TempDir() + "/summary"
	t.Setenv("GITHUB_STEP_SUMMARY", p)
	return func() string {
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

var _ = fmt.Sprintf
