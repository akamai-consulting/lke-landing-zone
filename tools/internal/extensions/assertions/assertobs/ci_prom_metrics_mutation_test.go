package assertobs

import (
	"errors"
	"strings"
	"testing"
)

// The command's whole product is the metric names on stdout. The existing happy
// path only asserted err == nil, so both error guards inside the port-forward
// session could be inverted — short-circuiting before the names were collected,
// or reporting the reachable Prometheus as unreachable — and still "pass".
func TestPromMetricsPrintsTheMatchingNames(t *testing.T) {
	orig := WithPrometheus
	t.Cleanup(func() { WithPrometheus = orig })
	WithPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(string) ([]byte, error) {
			return []byte(`{"status":"success","data":["loki_b","up","loki_a","vault_x"]}`), nil
		})
	}
	var err error
	out := captureStdout(t, func() { err = runCIPromMetrics("^loki_", "monitoring/prometheus-operated:9090") })
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if out != "loki_a\nloki_b\n" {
		t.Errorf("stdout = %q, want the sorted matching names", out)
	}
}

// A failing query must surface as the unreachable-Prometheus notice (exit 0 with a
// where-it-looked hint), not as a silent empty result set.
func TestPromMetricsQueryErrorIsReported(t *testing.T) {
	orig := WithPrometheus
	t.Cleanup(func() { WithPrometheus = orig })
	WithPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(string) ([]byte, error) { return nil, errors.New("connection refused") })
	}
	var err error
	var out string
	errOut := captureStderr(t, func() {
		out = captureStdout(t, func() { err = runCIPromMetrics(".", "monitoring/bogus:9090") })
	})
	if err != nil {
		t.Fatalf("a failed query is non-fatal, got %v", err)
	}
	if !strings.Contains(errOut, "could not reach Prometheus at monitoring/bogus:9090") ||
		!strings.Contains(errOut, "connection refused") {
		t.Errorf("stderr must name the target and the cause:\n%s", errOut)
	}
	if out != "" {
		t.Errorf("a failed query must print no names, got %q", out)
	}
}
