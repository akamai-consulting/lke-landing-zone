package main

import (
	"errors"
	"regexp"
	"testing"
)

func TestParseAlertRulesSkipsRecordingAndNonMatching(t *testing.T) {
	raw := []byte(`{"items":[
	  {"metadata":{"namespace":"llz-reconciler"},"spec":{"groups":[
	    {"name":"g1","rules":[
	      {"alert":"LLZOpenBaoSealed","expr":"max(llz_openbao_sealed) == 1"},
	      {"record":"job:foo","expr":"sum(rate(x[5m]))"},
	      {"alert":"KubeSomethingElse","expr":"up == 0"}
	    ]}
	  ]}}
	]}`)
	got := parseAlertRules(raw, regexp.MustCompile("^(LLZ|OpenBao)"))
	if len(got) != 1 || got[0].Alert != "LLZOpenBaoSealed" {
		t.Fatalf("expected only LLZOpenBaoSealed (recording rule + non-matching dropped), got %+v", got)
	}
	if got[0].Namespace != "llz-reconciler" || got[0].Expr == "" {
		t.Errorf("rule fields not populated: %+v", got[0])
	}
}

func TestExprMetricsExist(t *testing.T) {
	known := map[string]bool{"llz_openbao_sealed": true, "loki_request_duration_seconds_count": true}
	cases := []struct {
		expr string
		want bool
	}{
		{`max(llz_openbao_sealed{namespace="llz-reconciler"}) == 1`, true}, // real metric present
		{`rate(loki_request_duration_seconds_count[5m]) > 0`, true},        // real metric present
		{`max(llz_credential_age_days{cred="loki"}) > 90`, false},          // metric absent → DEAD?
		{`time() - max(nonexistent_metric) > 300`, false},                  // absent → DEAD?
	}
	for _, c := range cases {
		if got := exprMetricsExist(c.expr, known); got != c.want {
			t.Errorf("exprMetricsExist(%q) = %v, want %v", c.expr, got, c.want)
		}
	}
	// Empty known set must not claim DEAD? (we can't know).
	if !exprMetricsExist("max(anything) > 0", map[string]bool{}) {
		t.Error("empty known set should return true (cannot determine absence)")
	}
}

func TestClassifyAlertEval(t *testing.T) {
	known := map[string]bool{"llz_openbao_sealed": true}
	r := evalRule{Namespace: "llz-reconciler", Alert: "LLZOpenBaoSealed", Expr: "max(llz_openbao_sealed) == 1"}

	firing := []byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"1"]}]}}`)
	if v := classifyAlertEval(r, firing, nil, known); v.verdict != "FIRING" || v.value != "1" {
		t.Errorf("non-empty result → FIRING value=1, got %+v", v)
	}

	empty := []byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`)
	if v := classifyAlertEval(r, empty, nil, known); v.verdict != "ARMED" {
		t.Errorf("empty + metric present → ARMED, got %+v", v)
	}

	// Empty result whose named metric is absent → DEAD? (the silent never-fire signature).
	rDead := evalRule{Alert: "LLZCredentialRotationOverdue", Expr: "max(llz_credential_age_days) > 90"}
	if v := classifyAlertEval(rDead, empty, nil, known); v.verdict != "DEAD?" {
		t.Errorf("empty + metric absent → DEAD?, got %+v", v)
	}

	promErr := []byte(`{"status":"error","error":"parse error: unexpected }"}`)
	if v := classifyAlertEval(r, promErr, nil, known); v.verdict != "BROKEN" {
		t.Errorf("status=error → BROKEN, got %+v", v)
	}

	if v := classifyAlertEval(r, nil, errors.New("connection refused"), known); v.verdict != "BROKEN" {
		t.Errorf("query error → BROKEN, got %+v", v)
	}
}

func TestAlertEvalBadRegex(t *testing.T) {
	if err := runCIAlertEval("(", "monitoring/prometheus-operated:9090", false); err == nil {
		t.Error("invalid --match must error")
	}
}

func TestAlertEvalUnreachableNonFatal(t *testing.T) {
	orig := execOutput
	t.Cleanup(func() { execOutput = orig })
	execOutput = func(_ string, _ ...string) ([]byte, error) { return nil, errors.New("no cluster") }
	if err := runCIAlertEval(".", "monitoring/prometheus-operated:9090", false); err != nil {
		t.Errorf("unreachable cluster must be non-fatal, got %v", err)
	}
}
