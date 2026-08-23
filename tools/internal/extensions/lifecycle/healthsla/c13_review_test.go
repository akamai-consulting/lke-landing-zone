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

// TestTheProductionBaoExecArgvKeepsTheTokenOffTheCommandLine — the sibling seam.
// BaoExecArgv is wired straight to openbao.ExecArgv, and the argv it builds is
// handed to `kubectl`, so the token's position decides whether it appears in a
// process listing on the runner.
func TestTheProductionBaoExecArgvKeepsTheTokenOffTheCommandLine(t *testing.T) {
	const token = "s.a-real-token"
	argv := HealthSLADeps().BaoExecArgv("platform-openbao-0", token, []string{"kv", "metadata", "get", "secret/x"})
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "platform-openbao-0") {
		t.Fatalf("the pod must reach kubectl: %v", argv)
	}
	if !strings.Contains(joined, "secret/x") {
		t.Errorf("the bao args must be forwarded: %v", argv)
	}
	// The token has to reach the pod somehow; what it must NOT be is a bare
	// positional sitting where `bao`'s own args go.
	for i, a := range argv {
		if a == token && (i == 0 || !strings.Contains(argv[i-1], "TOKEN")) {
			t.Errorf("the token appears at argv[%d] as a bare positional (%v) — it belongs in the "+
				"env assignment, not somewhere a process listing reads it back", i, argv)
		}
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

// ── absence is not unreadability ─────────────────────────────────────────────

// TestAnUnseededLokiKeyIsNotAHardFailure.
//
// `bao kv metadata get` reports "not there" by EXITING 2 with "No value found
// at …" on stderr — it does not return an empty document. Folded into the
// read-failure arm, an instance that simply has not seeded Loki yet failed the
// weekly job outright, and the `Updated == ""` branch that carries the "seed it
// via bootstrap-openbao.yml" remedy became unreachable against real bao,
// satisfiable only by a `{"data":{}}` shape nothing produces.
func TestAnUnseededLokiKeyIsNotAHardFailure(t *testing.T) {
	read := withSummaryFile(t)
	ensureDeps(t)
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.tok")
	stubKubectl(t, func([]string) ([]byte, error) {
		return nil, errors.New("Error reading secret/metadata/loki/object-store: No value found at secret/metadata/loki/object-store")
	})

	var err error
	captureStdout(t, func() { err = RunLokiObjkeyRotation(td, 105, 120) })
	if err != nil {
		t.Fatalf("an unseeded Loki key is an action item, not a failed gate: %v", err)
	}
	if !strings.Contains(read(), "Seed it via bootstrap-openbao.yml") {
		t.Errorf("the summary must carry the seeding remedy, got:\n%s", read())
	}
}

// TestAGenuineReadFailureStillFails pins the exclusion: an RBAC denial or an
// unreachable pod is NOT an absence, and grading it as one is how a stale
// credential ages past its SLA unremarked.
func TestAGenuineReadFailureStillFails(t *testing.T) {
	withSummaryFile(t)
	ensureDeps(t)
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.tok")
	stubKubectl(t, func([]string) ([]byte, error) {
		return nil, errors.New("error dialing backend: No agent available")
	})

	var err error
	captureStdout(t, func() { err = RunLokiObjkeyRotation(td, 105, 120) })
	if err == nil {
		t.Error("a read that failed for a reason other than absence rendered no verdict, and no verdict " +
			"is not a pass on a hard SLA gate")
	}
}

// TestCertManagerDoesNotReportAllReadyOverZeroCertificates.
//
// kubectlprobe treats "the server doesn't have a resource type" — the
// cert-manager CRDs are not installed — as an ANSWERED absence, so `listed` is
// true, the loop never runs, notReady stays 0, and the check printed "All
// cert-manager Certificates Ready" on a cluster with no cert-manager at all.
// That is the same false all-clear the ItemsOK conversion removed, one branch
// further on.
func TestCertManagerDoesNotReportAllReadyOverZeroCertificates(t *testing.T) {
	read := withSummaryFile(t)
	ensureDeps(t)
	stubKubectl(t, func([]string) ([]byte, error) { return itemsJSON(), nil })

	var err error
	captureStdout(t, func() { err = RunCertManager(td) })
	if err != nil {
		t.Fatalf("a cluster without cert-manager is not a broken one: %v", err)
	}
	body := read()
	if strings.Contains(body, "All Certificates Ready") {
		t.Errorf("reported every Certificate Ready having examined none:\n%s", body)
	}
	if !strings.Contains(body, "No Certificates found") {
		t.Errorf("the summary must say nothing was examined, got:\n%s", body)
	}
}

// TestExternalSecretsDoesNotReportAllReadyOverZeroItems — the same hole in the
// branch RunCertManager's own comment cites as its model.
func TestExternalSecretsDoesNotReportAllReadyOverZeroItems(t *testing.T) {
	read := withSummaryFile(t)
	ensureDeps(t)
	stubBaoExec(t, func(string, []string) (string, error) {
		return `{"initialized":true,"sealed":false,"is_self":true,"ha_enabled":true}`, nil
	})
	stubKubectl(t, func(args []string) ([]byte, error) {
		if argsContain(args, "clustersecretstores") {
			return []byte("True"), nil
		}
		return itemsJSON(), nil
	})

	captureStdout(t, func() {
		if err := RunOpenbao(td); err != nil {
			t.Fatal(err)
		}
	})
	if body := read(); strings.Contains(body, "All ExternalSecrets: Ready") {
		t.Errorf("reported every ExternalSecret Ready having examined none:\n%s", body)
	}
}
