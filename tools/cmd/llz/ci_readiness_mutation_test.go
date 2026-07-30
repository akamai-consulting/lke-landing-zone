package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The retry line is the only progress an operator sees while the settle budget
// burns; its counter has to advance so a stuck gate reads as "attempt 7", not
// as the same line over and over (or a countdown into negative attempts).
func TestRunCIAssertLokiAttemptCounterAdvances(t *testing.T) {
	withKubectl(t, func(string) ([]byte, error) { return nil, errors.New("apiserver 503") })

	out := captureStdout(t, func() {
		if err := runCIAssertLoki("loki", 200*time.Millisecond, 20*time.Millisecond); err == nil {
			t.Error("an unbootstrapped Loki must still fail after the settle budget")
		}
	})
	for _, want := range []string{"attempt 1:", "attempt 2:", "attempt 3:"} {
		if !strings.Contains(out, want) {
			t.Errorf("retry log missing %q — the attempt counter does not advance:\n%s", want, out)
		}
	}
}

// The Argo Application block is non-gating but it is the operator's pointer at
// WHY Loki is unhealthy, so it has to report the app's real sync/health rather
// than skipping the app (a parse that succeeded) or inverting the verdict.
func TestRunCIAssertLokiReportsArgoApplicationState(t *testing.T) {
	stub := func(sync, health string) func(string) ([]byte, error) {
		return func(a string) ([]byte, error) {
			switch a {
			case "get pods -A -l app.kubernetes.io/name=loki -o json":
				return items(`{"metadata":{"namespace":"observability","name":"loki-0"},"status":{"phase":"Running","containerStatuses":[{"name":"loki","ready":true}]}}`), nil
			case "get configmap -A -o json":
				return items(`{"metadata":{"name":"loki-config"},"data":{"config.yaml":"object_store: s3\n"}}`), nil
			case "get crd applications.argoproj.io":
				return nil, nil // the CRD exists
			case "get applications.argoproj.io -A -o json":
				return items(`{"metadata":{"name":"loki"},"status":{"sync":{"status":"` + sync + `"},"health":{"status":"` + health + `"}}}`), nil
			}
			return nil, errors.New("unexpected: " + a)
		}
	}

	withKubectl(t, stub("Synced", "Healthy"))
	out := captureStdout(t, func() {
		if err := runCIAssertLoki("loki", 0, 0); err != nil {
			t.Fatalf("a bootstrapped Loki must pass: %v", err)
		}
	})
	if !strings.Contains(out, "OK: Argo Application loki Synced + Healthy") {
		t.Errorf("a Synced+Healthy Application must be reported OK:\n%s", out)
	}

	withKubectl(t, stub("OutOfSync", "Degraded"))
	out = captureStdout(t, func() {
		if err := runCIAssertLoki("loki", 0, 0); err != nil {
			t.Fatalf("the Argo block is non-gating: %v", err)
		}
	})
	if !strings.Contains(out, "WARN: Argo Application loki sync=OutOfSync health=Degraded") {
		t.Errorf("an unhealthy Application must be reported WARN:\n%s", out)
	}
}
