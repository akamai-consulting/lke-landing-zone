package assertobs

import (
	"errors"
	"strings"
	"testing"
)

// THE RULE THAT COULD NOT FIRE, as it actually shipped. This is the case the
// whole file exists for: the metric name is real, the label is not, and every
// name-level check grades it healthy.
const brokenLokiExpr = `kube_statefulset_status_replicas_ready{namespace="monitoring", statefulset="loki"} == 0`

// And the fixed one — the shape the rule actually ships as, comparing ready
// against DESIRED (kube_statefulset_replicas). Kept in step with the real rule so
// this fixture cannot drift into testing an expression nobody deploys.
const fixedLokiExpr = `kube_statefulset_status_replicas_ready{namespace="monitoring", statefulset=~"loki.*"}
< kube_statefulset_replicas{namespace="monitoring", statefulset=~"loki.*"}`

// liveSeries fakes a cluster: selectors listed here match, everything else does
// not. Written as the QUESTION the probe asks (the selector string), so a change
// in how selectors are rendered shows up as a test failure rather than silently
// matching nothing.
func liveSeries(present ...string) selectorMatcher {
	set := map[string]bool{}
	for _, p := range present {
		set[p] = true
	}
	return func(sel string) (bool, bool) { return set[sel], true }
}

func TestTheBrokenLokiSelectorIsCaught(t *testing.T) {
	// A cluster where the ingester StatefulSet exists under its real name.
	match := liveSeries(`kube_statefulset_status_replicas_ready{namespace="monitoring", statefulset=~"loki.*"}`)
	dead := unmatchedSelectors(brokenLokiExpr, match)
	if len(dead) == 0 {
		t.Fatal("the shipped-broken expr was not flagged — this is the 16-day outage, and the " +
			"name-level DEAD? check already grades it ARMED, so nothing else would say a word")
	}
	if !strings.Contains(dead[0], `statefulset="loki"`) {
		t.Errorf("the finding does not name the selector that matched nothing: %v", dead)
	}
}

func TestTheFixedLokiSelectorIsNotFlagged(t *testing.T) {
	match := liveSeries(
		`kube_statefulset_status_replicas_ready{namespace="monitoring", statefulset=~"loki.*"}`,
		`kube_statefulset_replicas{namespace="monitoring", statefulset=~"loki.*"}`,
	)
	if dead := unmatchedSelectors(fixedLokiExpr, match); len(dead) != 0 {
		t.Errorf("the corrected expr was flagged: %v", dead)
	}
}

// A HEALTHY ERROR-RATE RULE MUST NOT BE FLAGGED. `sum(rate(errors))/sum(rate(all))`
// on a cluster with no errors has an empty numerator, which is the healthy state.
// Flagging it would put a NOMATCH line on every good cluster and the whole signal
// would be tuned out.
func TestAPartiallyEmptyExprIsNotAFinding(t *testing.T) {
	expr := `sum(rate(loki_request_duration_seconds_count{namespace="monitoring", status_code=~"5.."}[5m]))
/ sum(rate(loki_request_duration_seconds_count{namespace="monitoring"}[5m])) > 0.05`
	match := liveSeries(`loki_request_duration_seconds_count{namespace="monitoring"}`)
	if dead := unmatchedSelectors(expr, match); len(dead) != 0 {
		t.Errorf("a healthy error-rate rule (no 5xx yet) was flagged: %v", dead)
	}
}

// "COULD NOT ASK" IS NOT "NOTHING THERE". One unanswerable selector must suppress
// the whole finding: naming the others as dead while this one was never asked
// reads as a diagnosis and is a guess.
func TestAnUnanswerableProbeReportsNothing(t *testing.T) {
	var asked int
	match := func(string) (bool, bool) {
		asked++
		return false, false
	}
	if dead := unmatchedSelectors(brokenLokiExpr, match); len(dead) != 0 {
		t.Errorf("an unanswerable probe produced a finding: %v", dead)
	}
	if asked == 0 {
		t.Error("the probe was never called — the test proves nothing")
	}
}

// The selector extractor must not mistake PromQL functions and aggregators for
// metric names; querying `sum{...}` would produce a BROKEN-looking result for a
// perfectly good rule.
func TestSelectorExtractionSkipsFunctionsAndBareNames(t *testing.T) {
	for name, tc := range map[string]struct {
		expr string
		want []string
	}{
		"bare name is left to the name-level check": {`up == 0`, nil},
		"labelled selector": {
			`up{namespace="x"} == 0`,
			[]string{`up{namespace="x"}`},
		},
		"aggregation wrapper is not a metric": {
			`sum(rate(foo_total{ns="x"}[5m])) > 0`,
			[]string{`foo_total{ns="x"}`},
		},
		"duplicates collapse": {
			`a{x="1"} / a{x="1"}`,
			[]string{`a{x="1"}`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := alertSelectors(tc.expr)
			if len(got) != len(tc.want) {
				t.Fatalf("alertSelectors(%q) = %v, want %v", tc.expr, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("alertSelectors(%q)[%d] = %q, want %q", tc.expr, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// An expr with NO labelled selectors must produce no finding rather than a
// vacuous one — otherwise every `up == 0` style rule would be flagged forever.
func TestAnExprWithNoLabelledSelectorsIsSkipped(t *testing.T) {
	called := false
	if dead := unmatchedSelectors(`up == 0`, func(string) (bool, bool) { called = true; return false, true }); len(dead) != 0 {
		t.Errorf("a bare-name expr was flagged: %v", dead)
	}
	if called {
		t.Error("the probe was called for an expr with no labelled selector — that is a wasted round trip per rule")
	}
}

func TestSelectorHasSeriesSeparatesEmptyFromUnanswerable(t *testing.T) {
	for name, tc := range map[string]struct {
		raw              []byte
		err              error
		matches, answers bool
	}{
		"one series":        {[]byte(`{"status":"success","data":{"result":[{}]}}`), nil, true, true},
		"empty result":      {[]byte(`{"status":"success","data":{"result":[]}}`), nil, false, true},
		"prometheus error":  {[]byte(`{"status":"error","error":"bad"}`), nil, false, false},
		"unparseable":       {[]byte(`not json`), nil, false, false},
		"transport failure": {nil, errors.New("port-forward died"), false, false},
	} {
		t.Run(name, func(t *testing.T) {
			m, a := selectorHasSeries(tc.raw, tc.err)
			if m != tc.matches || a != tc.answers {
				t.Errorf("= (%v, %v), want (%v, %v)", m, a, tc.matches, tc.answers)
			}
		})
	}
}

// The operator-facing line must name the selector. "This alert is dead" and "the
// workload was renamed" have nothing in common as remedies, and the selector text
// is the only thing that distinguishes them.
func TestTheFindingNamesWhatWasAsked(t *testing.T) {
	got := selectorFinding("llz-observability/LokiStatefulSetUnavailable",
		[]string{`kube_statefulset_status_replicas_ready{statefulset="loki"}`})
	for _, want := range []string{"NOMATCH", "LokiStatefulSetUnavailable", `statefulset="loki"`, "never incremented"} {
		if !strings.Contains(got, want) {
			t.Errorf("finding is missing %q:\n%s", want, got)
		}
	}
}

// A HALF-DEAD COMPARISON IS STILL DEAD, and the first cut could not see it: it
// only reported when EVERY selector matched nothing, so a two-sided rule with one
// live side read ARMED. That is the exact shape of LokiStatefulSetDegraded, the
// rule this whole check was written for.
func TestAComparisonRuleWithOneDeadSideIsFlagged(t *testing.T) {
	expr := `kube_statefulset_status_replicas_ready{namespace="monitoring", statefulset=~"loki.*"}
< kube_statefulset_replicas{namespace="monitoring", statefulset="loki"}`
	// The ready side matches; the desired side names a StatefulSet that does not exist.
	match := liveSeries(`kube_statefulset_status_replicas_ready{namespace="monitoring", statefulset=~"loki.*"}`)
	dead := unmatchedSelectors(expr, match)
	if len(dead) == 0 {
		t.Fatal("a comparison with one dead side was not flagged — the rule cannot fire, and " +
			"'all selectors dead' reads it as healthy")
	}
	if !strings.Contains(dead[0], `statefulset="loki"`) {
		t.Errorf("the wrong side was named: %v", dead)
	}
}

// …AND THE ERROR-RATE SHAPE IS STILL EXEMPT, which is the whole reason the rule
// is not simply "any dead selector". Both sides name the SAME metric, and an
// empty numerator is the healthy state on a cluster with no errors.
func TestASameMetricSiblingStillExemptsAnEmptySelector(t *testing.T) {
	expr := `sum(rate(loki_request_duration_seconds_count{namespace="monitoring", status_code=~"5.."}[5m]))
/ sum(rate(loki_request_duration_seconds_count{namespace="monitoring"}[5m])) > 0.05`
	match := liveSeries(`loki_request_duration_seconds_count{namespace="monitoring"}`)
	if dead := unmatchedSelectors(expr, match); len(dead) != 0 {
		t.Errorf("a healthy error-rate rule was flagged: %v", dead)
	}
}
