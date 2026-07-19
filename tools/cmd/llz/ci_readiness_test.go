package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReplicasRolledOut(t *testing.T) {
	for in, want := range map[string]bool{
		"1/1":     true, // fully available
		"2/2":     true,
		"3/1":     true,  // more available than desired (surge) still counts
		"0/1":     false, // the fresh-bootstrap harbor-registry case
		"0/0":     false, // desired 0 → nothing to roll out, not "ready"
		"1/0":     false,
		"/":       false, // empty jsonpath (no availableReplicas yet)
		"":        false,
		"1":       false, // malformed
		"a/1":     false, // non-numeric
		" 1 / 1 ": true,  // whitespace tolerated
	} {
		if got := replicasRolledOut(in); got != want {
			t.Errorf("replicasRolledOut(%q) = %v, want %v", in, got, want)
		}
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
	origBudget := harborWaitBudget
	t.Cleanup(func() { harborWaitBudget = origBudget })

	// The verb waits for the harbor-registry rollout and nothing else. Its two
	// parameters are vestigial (kept so vendored instance workflows that still
	// pass --registry-only / --harbor-url keep parsing), so every combination
	// must behave identically — that equivalence is the point of the test.
	withKubectl(t, func(a string) ([]byte, error) {
		if strings.Contains(a, "get deployment harbor-registry") {
			return []byte("1/1"), nil // rolled out
		}
		return nil, errors.New("unexpected kubectl call: " + a)
	})
	for _, tc := range []struct {
		url          string
		registryOnly bool
	}{{"", false}, {"", true}, {"https://harbor.example", false}, {"https://harbor.example", true}} {
		if err := runCIWaitHarbor(tc.url, tc.registryOnly); err != nil {
			t.Errorf("rollout OK (url=%q registryOnly=%v) => err %v, want nil", tc.url, tc.registryOnly, err)
		}
	}

	// A registry that never rolls out is a SOFT gate: it warns and returns nil,
	// because the convergence gate is the hard check. Previously this was a single
	// 2m `rollout status` that hard-failed, which the caller then had to mask with
	// continue-on-error — painting a green check over a scary "timed out" that
	// carried no signal either way. Budget 0 so waitPoll evaluates once instead of
	// polling the real budget.
	harborWaitBudget = 0
	withKubectl(t, func(a string) ([]byte, error) {
		if strings.Contains(a, "get deployment harbor-registry") {
			return []byte("0/1"), nil // never becomes available
		}
		return nil, errors.New("nope")
	})
	if err := runCIWaitHarbor("", true); err != nil {
		t.Errorf("registry not rolled out is soft => err %v, want nil (warn only)", err)
	}
}
