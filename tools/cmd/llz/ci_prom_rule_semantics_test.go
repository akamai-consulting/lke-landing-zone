package main

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
