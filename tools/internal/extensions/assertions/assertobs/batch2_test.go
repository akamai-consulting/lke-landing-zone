package assertobs

// Tests that reference this package, split out of cmd/llz/ci_batch2_test.go by
// the same classify-then-move-by-line-range pass.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuleEvalErrors(t *testing.T) {
	body := []byte(`{"data":{"groups":[
		{"name":"g1","rules":[{"name":"r1","lastError":""},{"name":"r2","lastError":"boom"}]},
		{"name":"g2","rules":[{"lastError":"no metric"}]}
	]}}`)
	got := ruleEvalErrors(body)
	want := []string{"g1/r2: boom", "g2/?: no metric"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("ruleEvalErrors = %v, want %v", got, want)
	}
	if ruleEvalErrors([]byte("not json")) != nil {
		t.Error("bad JSON must yield no findings")
	}
}

// TestHealthPromRulesFailsClosedWhenUnreachable inverts what the old test
// asserted. It used to require a clean exit 0 when no Prometheus pod was found —
// and the pod lookup targeted llz-observability, where apl-core's Prometheus has
// never run. So the test PINNED the bug: the verb always took its skip path and
// the test called that correct.
func TestHealthPromRulesFailsClosedWhenUnreachable(t *testing.T) {
	withPrometheusStub(t, func(string, func(func(string) ([]byte, error)) error) error {
		return errors.New("no cluster")
	})
	err := runCIHealthPromRules("monitoring/prometheus-operated:9090")
	if err == nil {
		t.Fatal("an unreachable Prometheus must FAIL — a check that cannot ask has established nothing, and exit 0 reads as a color.Green rule set")
	}
	if !strings.Contains(err.Error(), "could not query") {
		t.Errorf("error should say what it could not do: %v", err)
	}
}

func TestHealthPromRulesReportsErrors(t *testing.T) {
	withPrometheusStub(t, func(prom string, fn func(func(string) ([]byte, error)) error) error {
		// The namespace regression this fixes: the default must be apl-core's
		// Prometheus in `monitoring`, not the llz-observability namespace that holds
		// only the ServiceMonitor/PrometheusRule CRs.
		if !strings.HasPrefix(prom, "monitoring/") {
			t.Errorf("prom = %q, want it to target the monitoring namespace", prom)
		}
		return fn(func(path string) ([]byte, error) {
			if path != "/api/v1/rules" {
				return nil, errors.New("unexpected path " + path)
			}
			return []byte(`{"data":{"groups":[{"name":"g","rules":[{"name":"r","lastError":"boom"}]}]}}`), nil
		})
	})
	sum := filepath.Join(t.TempDir(), "sum")
	t.Setenv("GITHUB_STEP_SUMMARY", sum)
	t.Setenv("REGION", "primary")

	if err := runCIHealthPromRules("monitoring/prometheus-operated:9090"); err != nil {
		t.Fatalf("health-prom-rules: %v", err)
	}
	b, _ := os.ReadFile(sum)
	if !strings.Contains(string(b), "g/r: boom") {
		t.Errorf("summary missing the evaluation error:\n%s", b)
	}
}

// ── diagnose-argocd ──────────────────────────────────────────────────────────

func TestHealthPromRulesRefusesVacuousGreen(t *testing.T) {
	tests := []struct {
		name, body, wantErr string
	}{
		{
			name:    "prometheus error envelope at HTTP 200",
			body:    `{"status":"error","error":"query engine unavailable"}`,
			wantErr: "query engine unavailable",
		},
		{
			name:    "zero rule groups loaded",
			body:    `{"status":"success","data":{"groups":[]}}`,
			wantErr: "ZERO rule groups",
		},
		{
			name:    "unparseable body",
			body:    `not json`,
			wantErr: "could not parse",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withPrometheusStub(t, func(_ string, fn func(func(string) ([]byte, error)) error) error {
				return fn(func(string) ([]byte, error) { return []byte(tt.body), nil })
			})
			t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(t.TempDir(), "sum"))
			t.Setenv("REGION", "primary")
			err := runCIHealthPromRules("monitoring/prometheus-operated:9090")
			if err == nil {
				t.Fatalf("must fail rather than report healthy rules it never observed (body: %s)", tt.body)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}

	// A real, loaded rule set still passes.
	t.Run("loaded groups with no lastError pass", func(t *testing.T) {
		withPrometheusStub(t, func(_ string, fn func(func(string) ([]byte, error)) error) error {
			return fn(func(string) ([]byte, error) {
				return []byte(`{"status":"success","data":{"groups":[{"name":"g","rules":[{"name":"r"}]}]}}`), nil
			})
		})
		t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(t.TempDir(), "sum"))
		t.Setenv("REGION", "primary")
		if err := runCIHealthPromRules("monitoring/prometheus-operated:9090"); err != nil {
			t.Errorf("a loaded rule set with no errors must pass, got %v", err)
		}
	})
}
