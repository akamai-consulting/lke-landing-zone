package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHarborPingOK(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2.0/ping" {
			t.Errorf("pinged %q, want /api/v2.0/ping", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer good.Close()
	if !harborPingOK(good.URL + "/") { // trailing slash trimmed
		t.Error("200 ping should be OK")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer bad.Close()
	if harborPingOK(bad.URL) {
		t.Error("503 ping should not be OK")
	}
	if harborPingOK("http://127.0.0.1:0") {
		t.Error("unreachable ping should not be OK")
	}
}

func TestRunCIAssertLoki(t *testing.T) {
	// All checks pass: a Ready Loki pod (by label) + an S3-backed config + a
	// Synced/Healthy Argo app.
	withKubectl(t, func(a string) ([]byte, error) {
		switch a {
		case "get pods -A -l app.kubernetes.io/name=loki -o json":
			return items(`{"metadata":{"namespace":"observability","name":"loki-0"},"status":{"phase":"Running","containerStatuses":[{"name":"loki","ready":true}]}}`), nil
		case "get configmap -A -o json":
			return items(`{"metadata":{"name":"loki-config"},"data":{"config.yaml":"storage_config:\n  aws:\n    s3: s3://bucket\n  object_store: s3\n"}}`), nil
		case "get crd applications.argoproj.io":
			return nil, nil
		case "get applications.argoproj.io -A -o json":
			return items(`{"metadata":{"name":"loki"},"spec":{"syncPolicy":{"automated":{}}},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}}`), nil
		}
		return nil, errors.New("nope")
	})
	// settle=0 → single evaluation (no polling/sleep in the unit test).
	if ec := runCIAssertLoki("loki", 0, 0); ec != 0 {
		t.Errorf("bootstrapped Loki => exit %d, want 0", ec)
	}

	// No pods + filesystem config => fail (exit 1).
	withKubectl(t, func(a string) ([]byte, error) {
		switch a {
		case "get pods -A -l app.kubernetes.io/name=loki -o json", "get pods -A -o json":
			return items(), nil
		case "get configmap -A -o json":
			return items(`{"metadata":{"name":"loki-config"},"data":{"c":"object_store: filesystem\n"}}`), nil
		}
		return nil, errors.New("nope")
	})
	if ec := runCIAssertLoki("loki", 0, 0); ec != 1 {
		t.Errorf("unbootstrapped Loki => exit %d, want 1", ec)
	}
}

// A transient kubectl/apiserver blip on the first attempt must NOT fail the gate:
// the settle poll re-evaluates and passes once the blip clears. Regression test for
// the one-shot-gate flake (kItems collapses a kubectl error to "no items").
func TestRunCIAssertLokiRidesOutTransient(t *testing.T) {
	var n int
	withKubectl(t, func(a string) ([]byte, error) {
		n++
		if n <= 3 { // attempt 1's reads (pods label, pods fallback, config) all error
			return nil, errors.New("transient: apiserver 503")
		}
		switch a { // attempt 2 onward: healthy + S3-backed
		case "get pods -A -l app.kubernetes.io/name=loki -o json":
			return items(`{"metadata":{"namespace":"observability","name":"loki-0"},"status":{"phase":"Running","containerStatuses":[{"name":"loki","ready":true}]}}`), nil
		case "get configmap -A -o json":
			return items(`{"metadata":{"name":"loki-config"},"data":{"config.yaml":"object_store: s3\n"}}`), nil
		}
		return nil, nil // Argo CRD absent → non-gating block skipped
	})
	// Tiny interval so the retry is instant; settle large enough for attempt 2.
	if ec := runCIAssertLoki("loki", 2*time.Second, time.Millisecond); ec != 0 {
		t.Errorf("a first-attempt transient should be ridden out => exit %d, want 0 (calls=%d)", ec, n)
	}
}

func TestRunCIWaitHarbor(t *testing.T) {
	orig := harborRollout
	harborRollout = func(string) error { return nil } // stub: rollouts succeed
	t.Cleanup(func() { harborRollout = orig })

	// Secret present, rollouts OK, no URL => exit 0 (API ping skipped).
	withKubectl(t, func(a string) ([]byte, error) {
		if a == "-n harbor get secret harbor-admin-password" {
			return nil, nil
		}
		return nil, errors.New("nope")
	})
	if ec := runCIWaitHarbor("", false); ec != 0 {
		t.Errorf("ready Harbor (no URL) => exit %d, want 0", ec)
	}

	// registry-only: rolls out harbor-registry without touching the admin
	// Secret/control-plane checks => exit 0 on success.
	if ec := runCIWaitHarbor("", true); ec != 0 {
		t.Errorf("registry-only ready => exit %d, want 0", ec)
	}

	// A failing rollout => exit 1 (both the full gate and registry-only).
	harborRollout = func(string) error { return errors.New("timed out") }
	if ec := runCIWaitHarbor("", false); ec != 1 {
		t.Errorf("rollout timeout => exit %d, want 1", ec)
	}
	if ec := runCIWaitHarbor("", true); ec != 1 {
		t.Errorf("registry-only rollout timeout => exit %d, want 1", ec)
	}
}
