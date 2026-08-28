package cli

// Alert-rule SEMANTICS, as opposed to syntax.
//
// check-prom-rules proves the expressions parse. That is not the property that
// matters here: a rule can be live, loaded, syntactically perfect and still not
// fire when the thing it is named for happens. The reconciler's registry never
// expires a gauge, so a lane that dies keeps serving its last good sample —
// every `== 1` / `== 0` alert fed by it goes quiet exactly when its input
// breaks, which reads as "everything is fine".
//
// So the rules that exist to catch that get executed, against the CRD the
// cluster actually loads, via promtool's rule unit tests. See
// testdata/promrules/reconciler_alerts_test.yaml for the scenarios.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// reconcilerRuleCRD is the PrometheusRule under test, repo-relative.
const reconcilerRuleCRD = "../../../platform-apl/components/llzReconciler/llz-reconciler/prometheusrule.yaml"

func TestCredentialAlertsMatchTheSinglePaneFilter(t *testing.T) {
	wf, err := os.ReadFile("../../../instance-template/.github/workflows/llz-scheduled-checks.yml")
	if err != nil {
		t.Fatalf("read scheduled-checks workflow: %v", err)
	}
	m := regexp.MustCompile(`alert-eval --match '([^']+)'`).FindSubmatch(wf)
	if m == nil {
		t.Fatal("could not find the alert-eval --match filter in the workflow — this guard would pass vacuously")
	}
	filter := regexp.MustCompile(string(m[1]))

	crd, err := os.ReadFile(reconcilerRuleCRD)
	if err != nil {
		t.Fatal(err)
	}
	names := regexp.MustCompile(`(?m)^\s*-\s*alert:\s*(LLZ\S+)`).FindAllStringSubmatch(string(crd), -1)
	if len(names) == 0 {
		t.Fatal("found no alert names in the PrometheusRule")
	}
	checked := 0
	for _, n := range names {
		name := n[1]
		// Only the credential family is in this job's remit; the reconciler's own
		// health alerts are read by a different check.
		if !strings.Contains(name, "Credential") && !strings.Contains(name, "Token") {
			continue
		}
		checked++
		if !filter.MatchString(name) {
			t.Errorf("alert %q is a credential alert but does not match the single-pane filter %q — "+
				"the daily job will never evaluate it. Name it LLZCredential…", name, m[1])
		}
	}
	if checked == 0 {
		t.Fatal("matched no credential alerts to check — the name heuristic has drifted")
	}
}

// supportPlaneRuleCRD is the other PrometheusRule this repo ships. Its alerts
// were evaluated by NOTHING until the support-plane alert-eval step was added:
// the credential single pane filters to `^LLZ…`, so the Loki/Harbor/Grafana/OTel
// rules were syntax-checked at PR time and never executed again.
const supportPlaneRuleCRD = "../../../platform-apl/components/observability/prometheus-rules/support-plane-alerts.yaml"

// THE FILTER IS THE COVERAGE, and that is the whole reason this test exists.
//
// An alert name is not cosmetic when a job selects rules by regex: a rule the
// filter misses is a rule nobody ever evaluates, and it looks identical to a rule
// that is fine. `LokiStatefulSetUnavailable` spent its life in that state for a
// different reason (a selector matching nothing) — this pins the OTHER way the
// same silence is produced.
//
// Derived from the shipped CRD, never a hand-written list, so a new support-plane
// alert named outside the filter fails here instead of being quietly unwatched.
func TestSupportPlaneAlertsMatchTheScheduledFilter(t *testing.T) {
	wf, err := os.ReadFile("../../../instance-template/.github/workflows/llz-scheduled-checks.yml")
	if err != nil {
		t.Fatalf("read scheduled-checks workflow: %v", err)
	}
	// The SECOND alert-eval filter in the workflow — the first is the credential
	// single pane. Matching all of them and requiring one to accept each alert is
	// what keeps this from breaking when a third job is added.
	ms := regexp.MustCompile(`alert-eval --match '([^']+)'`).FindAllSubmatch(wf, -1)
	if len(ms) < 2 {
		t.Fatal("fewer than two alert-eval --match filters in the workflow — the support-plane " +
			"step is gone, so nothing evaluates the Loki/Harbor/Grafana/OTel rules and this " +
			"guard would pass vacuously")
	}
	var filters []*regexp.Regexp
	for _, m := range ms {
		filters = append(filters, regexp.MustCompile(string(m[1])))
	}

	crd, err := os.ReadFile(supportPlaneRuleCRD)
	if err != nil {
		t.Fatal(err)
	}
	names := regexp.MustCompile(`(?m)^\s*-\s*alert:\s*(\S+)`).FindAllStringSubmatch(string(crd), -1)
	if len(names) == 0 {
		t.Fatal("found no alert names in the support-plane PrometheusRule")
	}
	for _, n := range names {
		name := n[1]
		matched := false
		for _, f := range filters {
			if f.MatchString(name) {
				matched = true
			}
		}
		if !matched {
			t.Errorf("alert %q is shipped but matches NO alert-eval filter in llz-scheduled-checks.yml — "+
				"no scheduled job will ever evaluate it, which is indistinguishable from it being "+
				"healthy. Either name it inside an existing filter or widen one.", name)
		}
	}
}
