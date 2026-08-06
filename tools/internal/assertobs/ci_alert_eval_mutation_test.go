package assertobs

// ci_alert_eval_mutation_test.go pins the "did we notice the problem" logic that
// mutation testing found unguarded: the two verdict lines in printAlertEval (which
// decide whether a run reports a problem AT ALL) and the guards in runCIAlertEval /
// parsePromLabelValues that decide whether any rule was evaluated in the first
// place. A surviving mutant here means alert-eval cannot tell firing from silent.

import (
	"fmt"
	"strings"
	"testing"
)

// alertEvalHint is the stderr line printAlertEval emits only when something is
// worth looking at (a DEAD? or a FIRING alert).
const alertEvalHint = "DEAD? = named metrics all absent"

// alertEvalRuleJSON is a one-alert `kubectl get prometheusrules -o json` payload;
// %s is the metric the expr names.
const alertEvalRuleJSON = `{"items":[{"metadata":{"namespace":"llz-reconciler"},"spec":{"groups":[` +
	`{"name":"g","rules":[{"alert":"LLZTokenExpiringSoon","expr":"%s < 14"}]}]}}]}`

// alertEvalSeams points runCIAlertEval at a fake cluster: rulesJSON is the
// PrometheusRule listing, names is the /label/__name__/values body, and every
// /query returns an empty (not-currently-true) vector.
func alertEvalSeams(t *testing.T, rulesJSON, names string) {
	t.Helper()
	withExecOutput(t, func(string, ...string) ([]byte, error) { return []byte(rulesJSON), nil })
	withPrometheusStub(t, func(_ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(path string) ([]byte, error) {
			if strings.Contains(path, "__name__") {
				return []byte(names), nil
			}
			return []byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`), nil
		})
	})
}

// TestAlertEvalStrictPassesOnlyAfterEvaluatingTheRules is the positive half of
// the fail-closed contract already pinned by TestAlertEvalStrictFailsClosed: when
// the listing DOES succeed and rules DO match, --strict must run them and pass —
// not divert into vacuous(). Both guards it crosses (the kubectl error check and
// the empty-rule-set check) were only ever exercised on their failing arm.
func TestAlertEvalStrictPassesOnlyAfterEvaluatingTheRules(t *testing.T) {
	alertEvalSeams(t,
		fmt.Sprintf(alertEvalRuleJSON, "llz_token_expiry_days"),
		`{"status":"success","data":["llz_token_expiry_days"]}`)

	var err error
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			err = runCIAlertEval("^LLZ", "monitoring/prometheus-operated:9090", "", true)
		})
	})
	if err != nil {
		t.Fatalf("strict run over one ARMED rule = %v, want nil", err)
	}
	if !strings.Contains(stdout, "llz-reconciler/LLZTokenExpiringSoon") {
		t.Errorf("the matching rule was never evaluated; stdout:\n%s", stdout)
	}
	if want := "1 alerts — FIRING=0 ARMED=1 DEAD?=0 BROKEN=0"; !strings.Contains(stderr, want) {
		t.Errorf("tally missing %q; stderr:\n%s", want, stderr)
	}
}

// TestAlertEvalDeadDetectionNeedsTheMetricNameList: DEAD? is decided by
// intersecting the expr's identifiers with the live metric-name set, so a
// metric-name list that parses to nothing silently zeroes the DEAD? count and
// --strict passes having found nothing.
func TestAlertEvalDeadDetectionNeedsTheMetricNameList(t *testing.T) {
	alertEvalSeams(t,
		fmt.Sprintf(alertEvalRuleJSON, "llz_metric_that_does_not_exist"),
		`{"status":"success","data":["llz_token_expiry_days"]}`)

	var err error
	var stderr string
	captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			err = runCIAlertEval("^LLZ", "monitoring/prometheus-operated:9090", "", true)
		})
	})
	if err == nil {
		t.Fatal("strict must FAIL: the rule's only named metric is absent from the live set (DEAD?)")
	}
	if !strings.Contains(err.Error(), "1 DEAD?") {
		t.Errorf("error should count the DEAD? alert: %v", err)
	}
	if want := "DEAD?=1"; !strings.Contains(stderr, want) {
		t.Errorf("tally missing %q; stderr:\n%s", want, stderr)
	}
}

// TestParsePromLabelValues covers both arms of the unmarshal guard: a well-formed
// body must yield its values (an empty return there disables DEAD? detection).
func TestParsePromLabelValues(t *testing.T) {
	got := parsePromLabelValues([]byte(`{"status":"success","data":["a_metric","b_metric"]}`))
	if len(got) != 2 || got[0] != "a_metric" || got[1] != "b_metric" {
		t.Errorf("parsePromLabelValues(valid) = %v, want [a_metric b_metric]", got)
	}
	if got := parsePromLabelValues([]byte(`{oops`)); got != nil {
		t.Errorf("parsePromLabelValues(malformed) = %v, want nil", got)
	}
}

// TestPrintAlertEvalVerdictMatrix walks the verdict lines one counter at a time.
// Each arm (DEAD?, FIRING, BROKEN) gates a different consequence, so a case where
// exactly one counter is non-zero is what distinguishes them; the all-zero case is
// what proves the run can still report GREEN — i.e. that the check is falsifiable
// in both directions rather than always-firing or always-silent.
func TestPrintAlertEvalVerdictMatrix(t *testing.T) {
	v := func(verdict string) evalVerdict {
		return evalVerdict{rule: evalRule{Namespace: "llz-reconciler", Alert: "LLZ" + verdict}, verdict: verdict}
	}
	cases := []struct {
		name          string
		out           []evalVerdict
		wantHint      bool // the "investigate this" stderr line
		wantStrictErr bool
	}{
		{"all ARMED — nothing to report", []evalVerdict{v("ARMED")}, false, false},
		{"nothing evaluated at all", nil, false, false},
		{"DEAD? alone", []evalVerdict{v("ARMED"), v("DEAD?")}, true, true},
		{"FIRING alone", []evalVerdict{v("ARMED"), v("FIRING")}, true, false},
		{"BROKEN alone", []evalVerdict{v("ARMED"), v("BROKEN")}, false, true},
	}
	for _, c := range cases {
		for _, strict := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/strict=%v", c.name, strict), func(t *testing.T) {
				var err error
				var stderr string
				captureStdout(t, func() {
					stderr = captureStderr(t, func() { err = printAlertEval(c.out, "", strict) })
				})
				if got := strings.Contains(stderr, alertEvalHint); got != c.wantHint {
					t.Errorf("hint printed = %v, want %v; stderr:\n%s", got, c.wantHint, stderr)
				}
				wantErr := strict && c.wantStrictErr
				if (err != nil) != wantErr {
					t.Fatalf("printAlertEval(strict=%v) err = %v, want error: %v", strict, err, wantErr)
				}
				if wantErr && !strings.Contains(err.Error(), "alert(s)") {
					t.Errorf("error should name the failing counts: %v", err)
				}
			})
		}
	}
}
