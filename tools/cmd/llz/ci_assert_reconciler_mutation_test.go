package main

import (
	"strings"
	"testing"
	"time"
)

// TestRunAssertReconcilerCountsAttemptsUp pins the retry counter in the progress
// line. The settle loop is the only place this gate reports progress from, and
// an operator reading "attempt 7/…" in a 30-minute e2e log uses it to tell a
// slow converge from a wedged one — a counter that does not climb makes every
// retry look like the first.
func TestRunAssertReconcilerCountsAttemptsUp(t *testing.T) {
	// up=0 forever, so the loop keeps retrying until the settle budget is spent.
	up0 := []byte(`{"status":"success","data":{"result":[{"value":[1,"0"]}]}}`)
	seamReconcilerProm(t, up0, up0)
	stubReconcilerLease(t, "", false)
	stubExecCombined(t, "")

	out := captureStdout(t, func() {
		// A few milliseconds of settle at a 1ms interval → several attempts, no
		// meaningful wall-clock cost.
		_ = runCIAssertReconciler("ns/svc:9090", "llz-reconciler", false, nil, 10, 8*time.Millisecond, time.Millisecond)
	})
	if !strings.Contains(out, "attempt 1:") {
		t.Fatalf("the first retry must be labelled attempt 1:\n%s", out)
	}
	if !strings.Contains(out, "attempt 2:") {
		t.Errorf("the attempt counter must INCREASE across retries — got:\n%s", out)
	}
	if strings.Contains(out, "attempt 0:") || strings.Contains(out, "attempt -") {
		t.Errorf("the attempt counter must never run backwards:\n%s", out)
	}
}

// TestLeaseLeaderFreshAtExactlyMaxAge pins the inclusive edge of the Lease
// freshness window. maxAge mirrors the elector's own leaseDuration: a Lease
// renewed exactly maxAge ago is one the elector still considers HELD, so
// treating it as stale would fail a gate on a leader that genuinely exists —
// precisely the gauge-lag false negative reconcilerLeaseLive exists to absorb.
func TestLeaseLeaderFreshAtExactlyMaxAge(t *testing.T) {
	now := time.Unix(2000, 0)
	at := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339Nano) }

	exact := []byte(`{"spec":{"holderIdentity":"pod-a","renewTime":"` + at(-30*time.Second) + `"}}`)
	if h, ok := leaseLeaderFresh(exact, now, 30*time.Second); !ok || h != "pod-a" {
		t.Errorf("a Lease renewed EXACTLY maxAge ago is still held by the elector; got (%q,%v), want (pod-a,true)", h, ok)
	}
	// One nanosecond past the window is stale — the boundary is inclusive, not open.
	past := []byte(`{"spec":{"holderIdentity":"pod-a","renewTime":"` + at(-30*time.Second-time.Nanosecond) + `"}}`)
	if _, ok := leaseLeaderFresh(past, now, 30*time.Second); ok {
		t.Error("a Lease renewed past maxAge must not be live")
	}
}

// TestDumpReconcilerDiagnosticsPrintsKubectlOutput: the dump exists because the
// e2e cluster is torn down seconds after the gate fails, so this stderr block is
// the ONLY surviving evidence. It must carry kubectl's real output through, and
// substitute the "(no output)" placeholder only when there is genuinely none.
func TestDumpReconcilerDiagnosticsPrintsKubectlOutput(t *testing.T) {
	stubExecCombined(t, "llz-reconciler-abc  1/1  Running  3  4m\n")
	got := captureStderr(t, func() { dumpReconcilerDiagnostics("my-ns") })
	if !strings.Contains(got, "llz-reconciler-abc") {
		t.Errorf("kubectl's output must reach the dump verbatim — that is the whole evidence:\n%s", got)
	}
	if strings.Contains(got, "(no output)") {
		t.Errorf("a probe that DID produce output must not be reported as empty:\n%s", got)
	}

	stubExecCombined(t, "")
	got = captureStderr(t, func() { dumpReconcilerDiagnostics("my-ns") })
	if !strings.Contains(got, "(no output)") {
		t.Errorf("an empty probe must show the placeholder so the label is not left dangling:\n%s", got)
	}
}
