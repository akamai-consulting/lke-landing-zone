package main

import (
	"errors"
	"reflect"
	"regexp"
	"testing"
)

func TestFilterPromMetricNames(t *testing.T) {
	raw := []byte(`{"status":"success","data":["loki_request_duration_seconds_count","up","otelcol_exporter_send_failed_spans_total","loki_request_duration_seconds_count","harbor_up","vault_core_unsealed"]}`)
	got := filterPromMetricNames(raw, regexp.MustCompile(`^(loki_|otelcol_|harbor_)`))
	want := []string{"harbor_up", "loki_request_duration_seconds_count", "otelcol_exporter_send_failed_spans_total"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filterPromMetricNames = %v, want %v (sorted + deduped + vault_/up excluded)", got, want)
	}
}

func TestFilterPromMetricNamesBadJSON(t *testing.T) {
	if got := filterPromMetricNames([]byte("not json"), regexp.MustCompile(".")); got != nil {
		t.Errorf("bad JSON should yield nil, got %v", got)
	}
}

// runCIPromMetrics must not fail when Prometheus is unreachable — it's a best-effort
// keep_cluster diagnostic; a wrong --prom should report and exit 0, not abort.
func TestPromMetricsUnreachableIsNonFatal(t *testing.T) {
	orig := promGet
	t.Cleanup(func() { promGet = orig })
	promGet = func(_, _ string) ([]byte, error) { return nil, errors.New("no cluster") }
	if err := runCIPromMetrics(".", "monitoring/bogus:9090"); err != nil {
		t.Errorf("unreachable Prometheus must be non-fatal, got %v", err)
	}
}

// The happy path reads names from promGet (the port-forward seam), filters, sorts.
func TestPromMetricsHappyPath(t *testing.T) {
	orig := promGet
	t.Cleanup(func() { promGet = orig })
	promGet = func(_, path string) ([]byte, error) {
		if path != "/api/v1/label/__name__/values" {
			t.Errorf("unexpected path %q", path)
		}
		return []byte(`{"status":"success","data":["loki_x","up","vault_y"]}`), nil
	}
	if err := runCIPromMetrics("^loki_", "monitoring/po-prometheus:9090"); err != nil {
		t.Errorf("happy path should succeed, got %v", err)
	}
}

func TestPromMetricsBadRegex(t *testing.T) {
	if err := runCIPromMetrics("(", "monitoring/prometheus-operated:9090"); err == nil {
		t.Error("an invalid --match regex must error")
	}
}
