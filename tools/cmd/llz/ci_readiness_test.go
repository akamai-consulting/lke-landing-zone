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
	if err := runCIAssertLoki("loki", 0, 0); err != nil {
		t.Errorf("bootstrapped Loki => err %v, want nil", err)
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
	if err := runCIAssertLoki("loki", 0, 0); err == nil {
		t.Errorf("unbootstrapped Loki => err %v, want non-nil", err)
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
	if err := runCIAssertLoki("loki", 2*time.Second, time.Millisecond); err != nil {
		t.Errorf("a first-attempt transient should be ridden out => err %v, want nil (calls=%d)", err, n)
	}
}

func TestRunCIWaitHarbor(t *testing.T) {
	orig := harborRollout
	t.Cleanup(func() { harborRollout = orig })

	// The verb waits for the harbor-registry rollout and nothing else. Its two
	// parameters are vestigial (kept so vendored instance workflows that still
	// pass --registry-only / --harbor-url keep parsing), so every combination
	// must behave identically — that equivalence is the point of the test.
	harborRollout = func(string) error { return nil }
	for _, tc := range []struct {
		url          string
		registryOnly bool
	}{{"", false}, {"", true}, {"https://harbor.example", false}, {"https://harbor.example", true}} {
		if err := runCIWaitHarbor(tc.url, tc.registryOnly); err != nil {
			t.Errorf("rollout OK (url=%q registryOnly=%v) => err %v, want nil", tc.url, tc.registryOnly, err)
		}
	}

	// A failing rollout fails the gate, again regardless of the vestigial args.
	harborRollout = func(string) error { return errors.New("timed out") }
	if err := runCIWaitHarbor("", false); err == nil {
		t.Errorf("rollout timeout => err %v, want non-nil", err)
	}
	if err := runCIWaitHarbor("https://harbor.example", true); err == nil {
		t.Errorf("rollout timeout (vestigial args set) => err %v, want non-nil", err)
	}
}
