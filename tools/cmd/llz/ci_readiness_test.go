package main

import (
	"errors"
	"testing"
	"time"
)

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
