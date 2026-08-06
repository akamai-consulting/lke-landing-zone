package assertreconciler

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestEvalReconcilerGaugeBlamesTheRightThing pins the message split: an absent
// series accuses the reconciler; a failed query must not.
func TestEvalReconcilerGaugeBlamesTheRightThing(t *testing.T) {
	const absentWhy = "the reconciler isn't reporting (pod down / not scraped)"

	absent := []byte(`{"status":"success","data":{"result":[]}}`)
	if g := evalReconcilerGauge("m", "q", absent, 1, absentWhy, "mismatch"); g.failWhy != absentWhy {
		t.Errorf("a genuinely absent series should keep its diagnosis, got %q", g.failWhy)
	}

	broken := []byte(`{"status":"error","error":"query timed out"}`)
	g := evalReconcilerGauge("m", "q", broken, 1, absentWhy, "mismatch")
	if g.failWhy == absentWhy {
		t.Error("a Prometheus query failure must NOT be reported as the reconciler being down — that sends the operator to a healthy pod")
	}
	if !strings.Contains(g.failWhy, "QUERY failure") || !strings.Contains(g.failWhy, "query timed out") {
		t.Errorf("failure should name the query and carry Prometheus's reason, got %q", g.failWhy)
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

// seamReconcilerProm makes deps.WithPrometheus answer the up/leader queries from the
// supplied raw bodies (matched by which metric the query names).
func seamReconcilerProm(t *testing.T, upBody, leaderBody []byte) {
	orig := deps.WithPrometheus
	t.Cleanup(func() { deps.WithPrometheus = orig })
	deps.WithPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
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
	if err := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", false, nil, 10, 30*time.Second, time.Second); err != nil {
		t.Errorf("expected no error when up=1 and leader=1, got %v", err)
	}
}

// stubExecCombined records every deps.ExecCombined call and returns reply, so a failed
// assertion's diagnostic dump can be exercised without shelling real kubectl.
func stubExecCombined(t *testing.T, reply string) *[][]string {
	orig := deps.ExecCombined
	t.Cleanup(func() { deps.ExecCombined = orig })
	var calls [][]string
	deps.ExecCombined = func(name string, args ...string) string {
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
	if err := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", false, nil, 10, 0, time.Second); err == nil {
		t.Errorf("expected an error when llz_reconcile_up=0, got %v", err)
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
	if err := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", false, nil, 10, 0, time.Second); err == nil {
		t.Errorf("expected an error when leader gauge is absent, got %v", err)
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
	if err := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", false, nil, 10, 0, time.Second); err != nil {
		t.Fatalf("expected no error when the gauge lags but the Lease is live, got %v", err)
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
	if err := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", false, nil, 10, 0, time.Second); err == nil {
		t.Fatalf("expected an error when leader=0 and the Lease has no live holder, got %v", err)
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
	if err := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", false, nil, 10, 0, time.Second); err == nil {
		t.Fatalf("expected an error when up=0 regardless of the Lease, got %v", err)
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
	if err := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", false, nil, 10, 30*time.Second, time.Second); err != nil {
		t.Fatalf("expected exit 0, got %v", err)
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
	orig := deps.WithPrometheus
	t.Cleanup(func() { deps.WithPrometheus = orig })
	deps.WithPrometheus = func(_ string, _ func(func(string) ([]byte, error)) error) error {
		return errors.New("port-forward failed")
	}
	if err := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", false, nil, 10, 0, time.Second); err == nil {
		t.Errorf("expected an error when Prometheus is unreachable, got %v", err)
	}
}

// ── per-lane freshness ───────────────────────────────────────────────────────

func TestLanesFromDeploymentArgs(t *testing.T) {
	got := lanesFromDeploymentArgs([]string{
		"reconcile", "--metrics-addr=:8080",
		"--reconcile-argo-nudge", "--reconcile-sc-demote=true",
		"--reconcile-volume-labels", "--reconcile-volume-tags=false",
		"--reconcile-unknown-future-lane",
	})
	want := []string{"argo-nudge", "observe", "sc-demote", "volume-labels"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
	// observe runs unconditionally, so an argless container still expects it —
	// otherwise a reconciler with every optional lane off would be gated on
	// nothing at all and pass vacuously.
	if lanes := lanesFromDeploymentArgs(nil); len(lanes) != 1 || lanes[0] != alwaysOnReconcilerLane {
		t.Errorf("no args must still expect the always-on lane, got %v", lanes)
	}
	// An explicitly-disabled lane must NOT be demanded — an instance that turns a
	// lane off is not broken, and failing it here would make the gate unusable.
	for _, l := range lanesFromDeploymentArgs([]string{"--reconcile-volume-tags=false"}) {
		if l == "volume-tags" {
			t.Error("--reconcile-volume-tags=false must not be treated as enabled")
		}
	}
}

// TestReconcileFlagLaneTableMatchesReconcileGo is the coupling guard: the flag
// names and the lane names are declared independently in reconcile.go, and this
// gate's expected set is built from the mapping between them. If a lane is
// renamed on one side only, the gate silently stops demanding it — the exact
// failure mode the whole per-lane check exists to catch, one level up.
//
// THE PATH IS RELATIVE TO THIS PACKAGE AND POINTS ACROSS THE EXTRACTION BOUNDARY.
// reconcile.go is still package main's — `reconciler-runtime` has not been
// extracted yet — so the guard reads it where it lives. When that extraction
// happens this path moves with it, and a hard failure here is the correct
// outcome: a coupling guard that silently stops finding its subject is worse than
// one that breaks loudly.
func TestReconcileFlagLaneTableMatchesReconcileGo(t *testing.T) {
	const reconcileGo = "../../cmd/llz/reconcile.go"
	src, err := os.ReadFile(reconcileGo)
	if err != nil {
		t.Fatalf("reading %s: %v — if reconciler-runtime was extracted, this path moves with it", reconcileGo, err)
	}
	body := string(src)

	// Every --reconcile-* flag reconcile.go registers must be in the table.
	flagRe := regexp.MustCompile(`"(reconcile-[a-z-]+)"`)
	for _, m := range flagRe.FindAllStringSubmatch(body, -1) {
		flag := "--" + m[1]
		if _, ok := reconcileFlagLane[flag]; !ok {
			t.Errorf("reconcile.go registers %s but reconcileFlagLane has no entry — "+
				"the lane it enables will be silently excluded from assert-reconciler --lanes", flag)
		}
	}
	// And every lane name the table maps to must actually be registered.
	laneRe := regexp.MustCompile(`name:\s+"([a-z-]+)"`)
	registered := map[string]bool{}
	for _, m := range laneRe.FindAllStringSubmatch(body, -1) {
		registered[m[1]] = true
	}
	for flag, lane := range reconcileFlagLane {
		if !registered[lane] {
			t.Errorf("reconcileFlagLane maps %s to lane %q, which reconcile.go does not register — "+
				"the gate would demand a series nothing ever emits", flag, lane)
		}
	}
	if !registered[alwaysOnReconcilerLane] {
		t.Errorf("reconcile.go no longer registers the always-on lane %q", alwaysOnReconcilerLane)
	}
}

func TestEvalLaneFreshness(t *testing.T) {
	now := time.Unix(1_720_010_000, 0)
	expected := []string{"observe", "apl-overlay", "volume-labels", "ghost"}
	lastSuccess := map[string]float64{
		"observe":       float64(now.Add(-20 * time.Second).Unix()), // 30s cadence → fine
		"apl-overlay":   float64(now.Add(-2 * time.Hour).Unix()),    // 300s cadence → stale
		"volume-labels": float64(now.Add(-30 * time.Minute).Unix()), // 3600s cadence → fine
	}
	intervals := map[string]float64{"observe": 30, "apl-overlay": 300, "volume-labels": 3600}

	got := evalLaneFreshness(expected, lastSuccess, intervals, now, 10, defaultLaneInterval)
	byLane := map[string]laneVerdict{}
	for _, v := range got {
		byLane[v.Lane] = v
	}
	if byLane["observe"].FailWhy != "" {
		t.Errorf("observe should be fresh: %s", byLane["observe"].FailWhy)
	}
	if byLane["volume-labels"].FailWhy != "" {
		t.Errorf("a slow lane inside its OWN budget must pass — judging every lane on one cadence is the bug: %s",
			byLane["volume-labels"].FailWhy)
	}
	if byLane["apl-overlay"].FailWhy == "" {
		t.Error("a 300s lane silent for 2h must be stale")
	}
	// The whole point: a lane the Deployment enables that has NEVER reported is a
	// failure, not an absence. Passing here is how a dead lane stays invisible.
	if g := byLane["ghost"]; g.FailWhy == "" || g.Present {
		t.Errorf("an enabled lane with no series must FAIL closed, got %+v", g)
	}
}

// A lane reporting successes but no interval sample must still be judged — the
// fallback cadence is generous, so it can under-report staleness but never
// invent it.
func TestEvalLaneFreshnessFallsBackToDefaultInterval(t *testing.T) {
	now := time.Unix(1_720_010_000, 0)
	fresh := map[string]float64{"x": float64(now.Add(-time.Minute).Unix())}
	if v := evalLaneFreshness([]string{"x"}, fresh, nil, now, 10, defaultLaneInterval)[0]; v.FailWhy != "" {
		t.Errorf("a recent pass with no interval sample must pass: %s", v.FailWhy)
	}
	ancient := map[string]float64{"x": float64(now.Add(-100 * time.Hour).Unix())}
	if v := evalLaneFreshness([]string{"x"}, ancient, nil, now, 10, defaultLaneInterval)[0]; v.FailWhy == "" {
		t.Error("a lane silent for 100h must be stale even on the fallback cadence")
	}
}

// End-to-end through the seam: one dead lane among healthy ones must color.Red the
// gate. llz_reconcile_up is a max() across lanes, so it stays pinned at 1 — this
// is precisely what the aggregate gauges cannot see.
func TestRunAssertReconcilerFailsOnDeadLane(t *testing.T) {
	now := time.Now()
	one := []byte(`{"status":"success","data":{"result":[{"value":[1,"1"]}]}}`)
	success := []byte(`{"status":"success","data":{"resultType":"vector","result":[
	  {"metric":{"reconciler":"observe"},"value":[1,"` + strconv.FormatInt(now.Unix(), 10) + `"]}
	]}}`)
	intervals := []byte(`{"status":"success","data":{"resultType":"vector","result":[
	  {"metric":{"reconciler":"observe"},"value":[1,"30"]},
	  {"metric":{"reconciler":"volume-labels"},"value":[1,"3600"]}
	]}}`)

	orig := deps.WithPrometheus
	t.Cleanup(func() { deps.WithPrometheus = orig })
	deps.WithPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(path string) ([]byte, error) {
			switch {
			case strings.Contains(path, "last_success_timestamp"):
				return success, nil
			case strings.Contains(path, "interval_seconds"):
				return intervals, nil
			default:
				return one, nil
			}
		})
	}
	stubExecCombined(t, "")

	// volume-labels is enabled but has no last-success series — a dead lane.
	err := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", true,
		[]string{"observe", "volume-labels"}, 10, 0, time.Millisecond)
	if err == nil {
		t.Fatal("a dead lane must fail the gate even while up=1 and leader=1")
	}
	if !strings.Contains(err.Error(), "volume-labels") {
		t.Errorf("the failure must name the dead lane, got %v", err)
	}
}

// Failing to read the Deployment must FAIL, not silently degrade to an empty
// expected set — a gate that demands nothing reports nothing wrong.
func TestRunAssertReconcilerFailsWhenLaneSetUnknown(t *testing.T) {
	orig := enabledReconcilerLanes
	t.Cleanup(func() { enabledReconcilerLanes = orig })
	enabledReconcilerLanes = func(string) ([]string, error) { return nil, errors.New("no such deployment") }

	if err := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", true, nil, 10, 0, time.Millisecond); err == nil {
		t.Error("an unreadable Deployment must fail the gate, not skip the lane check")
	}
}
