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

func TestRunAssertReconcilerReportingDown(t *testing.T) {
	up0 := []byte(`{"status":"success","data":{"result":[{"value":[1,"0"]}]}}`)
	leader1 := []byte(`{"status":"success","data":{"result":[{"value":[1,"1"]}]}}`)
	seamReconcilerProm(t, up0, leader1)
	if code := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", 0, time.Second); code != 1 {
		t.Errorf("expected exit 1 when llz_reconcile_up=0, got %d", code)
	}
}

func TestRunAssertReconcilerNoLeaderOrAbsent(t *testing.T) {
	up1 := []byte(`{"status":"success","data":{"result":[{"value":[1,"1"]}]}}`)
	absent := []byte(`{"status":"success","data":{"result":[]}}`)
	// up=1 but leader series absent → fail.
	seamReconcilerProm(t, up1, absent)
	if code := runCIAssertReconciler("ns/svc:9090", "llz-reconciler", 0, time.Second); code != 1 {
		t.Errorf("expected exit 1 when leader gauge is absent, got %d", code)
	}
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
