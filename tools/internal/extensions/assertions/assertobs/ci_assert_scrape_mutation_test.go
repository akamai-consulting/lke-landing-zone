package assertobs

// Gap-closing tests for ci_assert_scrape.go surfaced by mutation testing. This
// gate exists because converge/health/assert-loki all stay color.Green while metrics
// silently stop flowing, so its own failure modes matter: refusing to pass
// vacuously must key on BOTH expectation lists being empty (not one), the
// rule-group verdict must only be claimed when rule groups were asserted, the
// retry loop must actually count its attempts, and a discovered-but-down target
// must carry the lastError that says WHY.

import (
	"strings"
	"testing"
	"time"
)

// stubPrometheus points the shared port-forward seam at canned /api/v1 payloads.
func stubPrometheus(t *testing.T, targets, rules string) {
	t.Helper()
	orig := WithPrometheus
	t.Cleanup(func() { WithPrometheus = orig })
	WithPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(path string) ([]byte, error) {
			if strings.HasPrefix(path, "/api/v1/targets") {
				return []byte(targets), nil
			}
			return []byte(rules), nil
		})
	}
}

const oneUpTarget = `{"data":{"activeTargets":[{"scrapePool":"serviceMonitor/n/m/0","health":"up"}]}}`

// An instance that overrides --monitors but asserts no rule groups (or vice
// versa) is a supported configuration — only asserting NOTHING is refused. And
// the run must not then claim a rule-group verdict it never checked.
func TestRunAssertScrapeAcceptsOneSidedExpectations(t *testing.T) {
	t.Run("monitors only", func(t *testing.T) {
		stubPrometheus(t, oneUpTarget, `{"data":{"groups":[]}}`)
		var err error
		out := captureStdout(t, func() {
			err = runCIAssertScrapeTargets("ns/svc:9090", []string{"n/m"}, nil, 0, time.Second)
		})
		if err != nil {
			t.Fatalf("monitors with no --rule-groups is a valid assertion, got %v", err)
		}
		if strings.Contains(out, "PrometheusRule group(s) loaded") {
			t.Errorf("no rule groups were asserted, so no rule-group verdict may be printed:\n%s", out)
		}
	})

	t.Run("rule groups only", func(t *testing.T) {
		stubPrometheus(t, `{"data":{"activeTargets":[]}}`, `{"data":{"groups":[{"name":"g","rules":[]}]}}`)
		var err error
		out := captureStdout(t, func() {
			err = runCIAssertScrapeTargets("ns/svc:9090", nil, []string{"g"}, 0, time.Second)
		})
		if err != nil {
			t.Fatalf("rule groups with no --monitors is a valid assertion, got %v", err)
		}
		if !strings.Contains(out, "OK: 1 PrometheusRule group(s) loaded") {
			t.Errorf("a loaded rule-group set must be reported:\n%s", out)
		}
	})
}

// ...but with NEITHER list there is nothing to assert, and passing would be
// vacuous — the gate must refuse before it ever reaches the cluster.
func TestRunAssertScrapeRefusesEmptyExpectations(t *testing.T) {
	orig := WithPrometheus
	t.Cleanup(func() { WithPrometheus = orig })
	reached := false
	WithPrometheus = func(string, func(func(string) ([]byte, error)) error) error {
		reached = true
		return nil
	}
	err := runCIAssertScrapeTargets("ns/svc:9090", nil, nil, 0, time.Second)
	if err == nil {
		t.Fatal("no monitors and no rule groups must not pass vacuously")
	}
	if !strings.Contains(err.Error(), "vacuously") {
		t.Errorf("err = %v, want the vacuous-pass refusal", err)
	}
	if reached {
		t.Error("the refusal must short-circuit before opening a port-forward")
	}
}

// The retry loop numbers its attempts for the operator watching a slow settle.
// A counter that does not advance makes a 12-attempt settle read as one attempt
// repeated (or worse, count backwards).
func TestRunAssertScrapeNumbersItsRetryAttempts(t *testing.T) {
	stubPrometheus(t, `{"data":{"activeTargets":[]}}`, `{"data":{"groups":[]}}`)
	var err error
	out := captureStdout(t, func() {
		err = runCIAssertScrapeTargets("ns/svc:9090", []string{"n/m"}, nil, 100*time.Millisecond, time.Millisecond)
	})
	if err == nil {
		t.Fatal("a never-discovered monitor must fail the gate")
	}
	for _, want := range []string{"attempt 1:", "attempt 2:", "attempt 3:"} {
		if !strings.Contains(out, want) {
			t.Errorf("retry log is missing %q — the attempt counter does not advance:\n%s", want, out)
		}
	}
}

// A discovered-but-down monitor is the "NetworkPolicy / port / TLS" arm, and its
// lastError is the only thing that distinguishes those three. Dropping it leaves
// the operator with "0/1 targets up" and nothing else.
func TestRunAssertScrapeReportsTheTargetLastError(t *testing.T) {
	const lastErr = "x509: certificate signed by unknown authority"
	stubPrometheus(t,
		`{"data":{"activeTargets":[{"scrapePool":"serviceMonitor/n/m/0","health":"down","lastError":"`+lastErr+`"}]}}`,
		`{"data":{"groups":[]}}`)
	var err error
	out := captureStdout(t, func() {
		err = runCIAssertScrapeTargets("ns/svc:9090", []string{"n/m"}, nil, 0, time.Second)
	})
	if err == nil {
		t.Fatal("a monitor with 0 up targets must fail the gate")
	}
	if !strings.Contains(out, "lastError: "+lastErr) {
		t.Errorf("the down target's lastError must be surfaced — it is what separates NetworkPolicy from port from TLS:\n%s", out)
	}
}
