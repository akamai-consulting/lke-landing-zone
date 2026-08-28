package assertobs

// alertselectors.go adds the check that would have caught
// `LokiStatefulSetUnavailable`: does this alert's LABEL SELECTOR match any series?
//
// WHY THE EXISTING DEAD? DETECTION COULD NOT SEE IT. DEAD? asks whether any metric
// NAME in the expr exists in the live metric set. The broken rule selected
// `kube_statefulset_status_replicas_ready{namespace="monitoring", statefulset="loki"}`
// on a cluster whose Loki StatefulSet is `loki-ingester`. The metric name exists —
// kube-state-metrics publishes it for every StatefulSet in the cluster — so the
// rule graded ARMED, which is the healthy verdict. The name was right and the
// LABEL was wrong, and name-level detection cannot tell those apart.
//
// This narrows the question from "does this metric exist anywhere" to "does this
// selector, labels included, match anything". A selector matching nothing is the
// signature of a workload that was renamed out from under a rule.
//
// WHY IT IS REPORTED AND NOT GATED, even under --strict. A selector legitimately
// matches nothing on a healthy cluster more often than it sounds: a counter that
// has never incremented publishes no series at all
// (`loki_discarded_samples_total` on a cluster that has discarded nothing is the
// obvious one), and so does an app that is deliberately not installed. Failing
// --strict on those would make the daily job red on healthy clusters, which ends
// with the job being ignored — the exact outcome this whole area is trying to
// avoid. So NOMATCH is loud in the output and absent from the exit status.
//
// The judgement is pure and the transport is injected, so the classification is
// testable without a cluster.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// promSelectorRe matches a `metric_name{...}` vector selector. Deliberately only
// selectors that CARRY labels: a bare `metric_name` is exactly what the existing
// name-level DEAD? check already covers, and re-asking it here would report the
// same fact twice under a different name.
//
// Not a PromQL parser. Pulling in Prometheus's own parser for this would be a
// heavy dependency for one question, and being approximate is safe HERE in a way
// it would not be in a gate: a selector this regex fails to extract is simply not
// checked (the alert keeps its existing verdict), and one it extracts wrongly
// produces a query Prometheus rejects, which is reported as unknown rather than as
// a finding. It can under-report; it cannot manufacture one.
var promSelectorRe = regexp.MustCompile(`([a-zA-Z_:][a-zA-Z0-9_:]*)\s*\{([^{}]*)\}`)

// promFuncNames are PromQL functions and aggregators that can be followed by a
// brace-looking construct, so `sum by (x) {…}`-shaped text does not read as a
// metric selector. Extracting a function name and querying it would produce a
// BROKEN-looking result for a perfectly good rule.
var promFuncNames = map[string]bool{
	"sum": true, "rate": true, "irate": true, "increase": true, "avg": true,
	"min": true, "max": true, "count": true, "count_values": true, "topk": true,
	"bottomk": true, "quantile": true, "stddev": true, "stdvar": true, "group": true,
	"absent": true, "absent_over_time": true, "delta": true, "idelta": true,
	"histogram_quantile": true, "label_replace": true, "label_join": true,
	"predict_linear": true, "deriv": true, "clamp_max": true, "clamp_min": true,
	"round": true, "vector": true, "time": true, "changes": true, "resets": true,
}

// alertSelectors returns the distinct labelled vector selectors in an expr, in a
// stable order so output and tests do not depend on map iteration.
func alertSelectors(expr string) []string {
	seen := map[string]bool{}
	for _, m := range promSelectorRe.FindAllStringSubmatch(expr, -1) {
		name, labels := m[1], strings.TrimSpace(m[2])
		if promFuncNames[name] || labels == "" {
			continue
		}
		seen[name+"{"+labels+"}"] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// selectorMatcher answers "does this selector match at least one series", and
// whether the question could be answered at all. Injected so the classification
// below is unit-testable.
type selectorMatcher func(selector string) (matches, answered bool)

// unmatchedSelectors returns the selectors in expr that match NO series and have
// no live sibling on the same metric. It returns nothing — never a finding — when
// any selector could not be evaluated: a partial answer here would name one
// selector as dead while another was simply unasked, which reads as a diagnosis
// and is a guess.
func unmatchedSelectors(expr string, match selectorMatcher) []string {
	sels := alertSelectors(expr)
	if len(sels) == 0 {
		return nil
	}
	var dead []string
	deadSet := map[string]bool{}
	for _, s := range sels {
		matches, answered := match(s)
		if !answered {
			return nil
		}
		if !matches {
			dead = append(dead, s)
			deadSet[s] = true
		}
	}
	if len(dead) == 0 {
		return nil
	}
	// NOT "all of them", and the first cut had it that way — which could not see
	// the shape it was written for. `LokiStatefulSetDegraded` compares
	// kube_statefulset_status_replicas_ready against kube_statefulset_replicas: two
	// selectors, and if one names a workload that does not exist the rule is just
	// as dead as if both did, while "all dead" reads it as ARMED and says nothing.
	//
	// The case that must NOT be flagged is different in a way the metric name
	// makes visible: an error-rate rule divides `X{err}` by `X{all}`, and an empty
	// numerator is the HEALTHY state on a cluster with no errors. Both selectors
	// name the SAME metric. So the rule is: a dead selector is a finding unless
	// another selector in the same expr names the same metric and DID match — that
	// is the error-rate shape, and only that.
	live := map[string]bool{}
	for _, s := range sels {
		if !deadSet[s] {
			live[selectorMetric(s)] = true
		}
	}
	var report []string
	for _, d := range dead {
		if live[selectorMetric(d)] {
			continue // a sibling selector on the same metric matched — error-rate shape
		}
		report = append(report, d)
	}
	return report
}

// selectorMetric is the metric name a selector opens with.
func selectorMetric(sel string) string {
	if i := strings.IndexByte(sel, '{'); i >= 0 {
		return sel[:i]
	}
	return sel
}

// selectorFinding renders the operator-facing line for an alert whose every
// selector is empty. It names what was asked, because "this alert is dead" and
// "we asked about a workload that was renamed" have nothing in common as remedies
// and the selector text is what distinguishes them.
func selectorFinding(alert string, dead []string) string {
	// "these selectors", not "every selector". The rule reports a SUBSET — a dead
	// selector with no live sibling on the same metric — and the message said
	// "every" from when it only fired if all of them were dead. On a two-sided
	// comparison with one live side that was simply false, and a finding that
	// misdescribes what it found sends the reader to check the wrong half.
	return fmt.Sprintf("NOMATCH  %s — %d selector(s) match ZERO series: %s. "+
		"The metric name exists, so this reads ARMED above; the LABELS are what select "+
		"nothing. A workload renamed out from under a rule looks exactly like this "+
		"(LokiStatefulSetUnavailable selected statefulset=\"loki\" on a cluster whose "+
		"StatefulSet is loki-ingester, and stayed quiet through a 16-day outage). "+
		"Confirm against the cluster before changing the rule — a counter that has "+
		"never incremented also matches nothing, and that is healthy.",
		alert, len(dead), strings.Join(dead, ", "))
}

// selectorHasSeries reads a `count(<selector>)` response. answered=false for a
// transport error or an unparseable/errored response — the caller then reports
// nothing, because "we could not ask" must not render as "there is nothing there".
func selectorHasSeries(raw []byte, err error) (matches, answered bool) {
	if err != nil {
		return false, false
	}
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct{} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &resp) != nil || resp.Status != "success" {
		return false, false
	}
	// count() over an empty selector returns an EMPTY result, not 0 — that is the
	// whole signal.
	return len(resp.Data.Result) > 0, true
}
