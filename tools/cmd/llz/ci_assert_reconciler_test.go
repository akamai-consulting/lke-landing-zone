package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPromScalar(t *testing.T) {
	one := []byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1720000000,"1"]}]}}`)
	if v, ok := promScalar(one); !ok || v != 1 {
		t.Errorf("expected (1,true), got (%v,%v)", v, ok)
	}
	zero := []byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1720000000,"0"]}]}}`)
	if v, ok := promScalar(zero); !ok || v != 0 {
		t.Errorf("expected (0,true), got (%v,%v)", v, ok)
	}
	empty := []byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`)
	if _, ok := promScalar(empty); ok {
		t.Error("empty result must report no series")
	}
	if _, ok := promScalar([]byte(`{"status":"error","error":"bad"}`)); ok {
		t.Error("non-success status must report no series")
	}
	if _, ok := promScalar([]byte(`not json`)); ok {
		t.Error("unparseable body must report no series")
	}
}

func TestEvalReconcilerGauge(t *testing.T) {
	up := []byte(`{"status":"success","data":{"result":[{"value":[1,"1"]}]}}`)
	down := []byte(`{"status":"success","data":{"result":[{"value":[1,"0"]}]}}`)
	absent := []byte(`{"status":"success","data":{"result":[]}}`)

	if g := evalReconcilerGauge("m", "q", up, 1, "absent", "mismatch"); g.failWhy != "" {
		t.Errorf("value=1 wanting 1 should pass: %+v", g)
	}
	if g := evalReconcilerGauge("m", "q", down, 1, "absent", "mismatch"); g.failWhy != "mismatch" {
		t.Errorf("value=0 wanting 1 should fail with mismatch: %+v", g)
	}
	if g := evalReconcilerGauge("m", "q", absent, 1, "absent", "mismatch"); g.failWhy != "absent" {
		t.Errorf("no series should fail with absent reason: %+v", g)
	}
}

func TestReconcilerProbeHealthy(t *testing.T) {
	ok := gaugeCheck{}
	bad := gaugeCheck{failWhy: "x"}
	if !(reconcilerProbe{up: ok, leader: ok}).healthy() {
		t.Error("both OK should be healthy")
	}
	if (reconcilerProbe{up: bad, leader: ok}).healthy() {
		t.Error("failing up gauge must be unhealthy")
	}
	if (reconcilerProbe{up: ok, leader: bad}).healthy() {
		t.Error("failing leader gauge must be unhealthy")
	}
}

// seamReconcilerProm makes withPrometheus answer the up/leader queries from the
// supplied raw bodies (matched by which metric the query names).
func seamReconcilerProm(t *testing.T, upBody, leaderBody []byte) {
	orig := withPrometheus
	t.Cleanup(func() { withPrometheus = orig })
	withPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(path string) ([]byte, error) {
			if strings.Contains(path, "llz_reconcile_leader") {
				return leaderBody, nil
			}
			return upBody, nil
		})
	}
}

func TestRunAssertReconcilerHealthy(t *testing.T) {
	one := []byte(`{"status":"success","data":{"result":[{"value":[1,"1"]}]}}`)
	seamReconcilerProm(t, one, one)
	if code := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", 30*time.Second, time.Second); code != 0 {
		t.Errorf("expected exit 0 when up=1 and leader=1, got %d", code)
	}
}

// stubExecCombined records every execCombined call and returns reply, so a failed
// assertion's diagnostic dump can be exercised without shelling real kubectl.
func stubExecCombined(t *testing.T, reply string) *[][]string {
	orig := execCombined
	t.Cleanup(func() { execCombined = orig })
	var calls [][]string
	execCombined = func(name string, args ...string) string {
		calls = append(calls, append([]string{name}, args...))
		return reply
	}
	return &calls
}

// stubReconcilerLease overrides the authoritative Lease read so the gauge→Lease
// fallback can be exercised without a cluster.
func stubReconcilerLease(t *testing.T, holder string, live bool) {
	orig := reconcilerLeaseLive
	t.Cleanup(func() { reconcilerLeaseLive = orig })
	reconcilerLeaseLive = func(string, time.Time) (string, bool) { return holder, live }
}

func TestRunAssertReconcilerReportingDown(t *testing.T) {
	up0 := []byte(`{"status":"success","data":{"result":[{"value":[1,"0"]}]}}`)
	leader1 := []byte(`{"status":"success","data":{"result":[{"value":[1,"1"]}]}}`)
	seamReconcilerProm(t, up0, leader1)
	calls := stubExecCombined(t, "")
	if code := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", 0, time.Second); code != 1 {
		t.Errorf("expected exit 1 when llz_reconcile_up=0, got %d", code)
	}
	if len(*calls) == 0 {
		t.Error("a failed assertion must dump reconciler diagnostics")
	}
}

func TestRunAssertReconcilerNoLeaderOrAbsent(t *testing.T) {
	up1 := []byte(`{"status":"success","data":{"result":[{"value":[1,"1"]}]}}`)
	absent := []byte(`{"status":"success","data":{"result":[]}}`)
	// up=1 but leader series absent, and the Lease has no live holder → real stall.
	seamReconcilerProm(t, up1, absent)
	stubReconcilerLease(t, "", false)
	calls := stubExecCombined(t, "")
	if code := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", 0, time.Second); code != 1 {
		t.Errorf("expected exit 1 when leader gauge is absent, got %d", code)
	}
	if len(*calls) == 0 {
		t.Error("a failed assertion must dump reconciler diagnostics")
	}
}

// The gauge is derived (process → 30s scrape → PromQL); the Lease is ground truth.
// leader=0 with a genuinely-held, fresh Lease is a scrape-lag false-negative — the
// gate must pass on the authoritative Lease and NOT dump diagnostics.
func TestRunAssertReconcilerLeaderGaugeLagsButLeaseLive(t *testing.T) {
	up1 := []byte(`{"status":"success","data":{"result":[{"value":[1,"1"]}]}}`)
	leader0 := []byte(`{"status":"success","data":{"result":[{"value":[1,"0"]}]}}`)
	seamReconcilerProm(t, up1, leader0)
	stubReconcilerLease(t, "llz-reconciler-abc123", true)
	calls := stubExecCombined(t, "")
	if code := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", 0, time.Second); code != 0 {
		t.Fatalf("expected exit 0 when the gauge lags but the Lease is live, got %d", code)
	}
	if len(*calls) != 0 {
		t.Errorf("a Lease-confirmed leader must NOT dump diagnostics, got %d calls", len(*calls))
	}
}

// leader=0 AND no live Lease holder is a real election stall — still fails + dumps.
func TestRunAssertReconcilerLeaderDownAndLeaseDead(t *testing.T) {
	up1 := []byte(`{"status":"success","data":{"result":[{"value":[1,"1"]}]}}`)
	leader0 := []byte(`{"status":"success","data":{"result":[{"value":[1,"0"]}]}}`)
	seamReconcilerProm(t, up1, leader0)
	stubReconcilerLease(t, "", false)
	calls := stubExecCombined(t, "")
	if code := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", 0, time.Second); code != 1 {
		t.Fatalf("expected exit 1 when leader=0 and the Lease has no live holder, got %d", code)
	}
	if len(*calls) == 0 {
		t.Error("a real no-leader stall must still dump diagnostics")
	}
}

// up=0 has no authoritative fallback: a failing sample loop fails even if a leader
// holds the Lease. The Lease must not be consulted to rescue an up failure.
func TestRunAssertReconcilerUpFailHasNoLeaseFallback(t *testing.T) {
	up0 := []byte(`{"status":"success","data":{"result":[{"value":[1,"0"]}]}}`)
	leader0 := []byte(`{"status":"success","data":{"result":[{"value":[1,"0"]}]}}`)
	seamReconcilerProm(t, up0, leader0)
	leaseConsulted := false
	orig := reconcilerLeaseLive
	t.Cleanup(func() { reconcilerLeaseLive = orig })
	reconcilerLeaseLive = func(string, time.Time) (string, bool) { leaseConsulted = true; return "holder", true }
	stubExecCombined(t, "")
	if code := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", 0, time.Second); code != 1 {
		t.Fatalf("expected exit 1 when up=0 regardless of the Lease, got %d", code)
	}
	if leaseConsulted {
		t.Error("the Lease fallback must not run when up is failing")
	}
}

func TestLeaseLeaderFresh(t *testing.T) {
	now := time.Unix(2000, 0)
	at := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339Nano) }

	fresh := []byte(`{"spec":{"holderIdentity":"pod-a","renewTime":"` + at(-5*time.Second) + `"}}`)
	if h, ok := leaseLeaderFresh(fresh, now, 30*time.Second); !ok || h != "pod-a" {
		t.Errorf("fresh held lease: got (%q,%v), want (pod-a,true)", h, ok)
	}
	stale := []byte(`{"spec":{"holderIdentity":"pod-a","renewTime":"` + at(-31*time.Second) + `"}}`)
	if _, ok := leaseLeaderFresh(stale, now, 30*time.Second); ok {
		t.Error("a stale renewTime must not be live")
	}
	released := []byte(`{"spec":{"holderIdentity":"","renewTime":"` + at(-1*time.Second) + `"}}`)
	if _, ok := leaseLeaderFresh(released, now, 30*time.Second); ok {
		t.Error("an empty holderIdentity (released) must not be live")
	}
	noRenew := []byte(`{"spec":{"holderIdentity":"pod-a"}}`)
	if _, ok := leaseLeaderFresh(noRenew, now, 30*time.Second); ok {
		t.Error("a missing renewTime must not be live")
	}
	if _, ok := leaseLeaderFresh([]byte(`not json`), now, 30*time.Second); ok {
		t.Error("an unparseable lease must not be live")
	}
}

func TestRunAssertReconcilerHealthyDoesNotDump(t *testing.T) {
	one := []byte(`{"status":"success","data":{"result":[{"value":[1,"1"]}]}}`)
	seamReconcilerProm(t, one, one)
	calls := stubExecCombined(t, "")
	if code := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", 30*time.Second, time.Second); code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if len(*calls) != 0 {
		t.Errorf("a healthy assertion must NOT dump diagnostics, got %d calls", len(*calls))
	}
}

func TestDumpReconcilerDiagnostics(t *testing.T) {
	calls := stubExecCombined(t, "") // every object "missing" → still one dump per probe
	dumpReconcilerDiagnostics("my-ns")

	if len(*calls) != 5 {
		t.Fatalf("expected 5 kubectl diagnostic probes, got %d: %v", len(*calls), *calls)
	}
	joined := make([]string, len(*calls))
	for i, c := range *calls {
		if c[0] != "kubectl" {
			t.Errorf("probe %d shelled %q, not kubectl", i, c[0])
		}
		if !containsArg(c, "-n") || !containsArg(c, "my-ns") {
			t.Errorf("probe %d not scoped to the reconciler namespace: %v", i, c)
		}
		joined[i] = strings.Join(c, " ")
	}
	all := strings.Join(joined, "\n")
	for _, want := range []string{
		"get pods",                         // restart counts
		"get lease llz-reconciler-leader",  // authoritative holder/renew
		"deploy/llz-reconciler --tail=100", // current logs
		"--previous",                       // crash logs
		"describe pods",                    // events
	} {
		if !strings.Contains(all, want) {
			t.Errorf("diagnostics missing a probe for %q\n%s", want, all)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestRunAssertReconcilerUnreachable(t *testing.T) {
	orig := withPrometheus
	t.Cleanup(func() { withPrometheus = orig })
	withPrometheus = func(_ string, _ func(func(string) ([]byte, error)) error) error {
		return errors.New("port-forward failed")
	}
	if code := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", 0, time.Second); code != 1 {
		t.Errorf("expected exit 1 when Prometheus is unreachable, got %d", code)
	}
}

func TestGaugeStr(t *testing.T) {
	if got := gaugeStr(0, false); got != "<absent>" {
		t.Errorf("absent series = %q, want <absent>", got)
	}
	if got := gaugeStr(0, true); got != "0" {
		t.Errorf("present zero = %q, want 0 (distinct from absent)", got)
	}
	if got := gaugeStr(3, true); got != "3" {
		t.Errorf("present 3 = %q, want 3", got)
	}
}
