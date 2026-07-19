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
	"os/exec"
	"path/filepath"
	"testing"
)

// reconcilerRuleCRD is the PrometheusRule under test, repo-relative.
const reconcilerRuleCRD = "../../../platform-apl/components/llzReconciler/llz-reconciler/prometheusrule.yaml"

func TestReconcilerAlertSemantics(t *testing.T) {
	promtool, err := exec.LookPath("promtool")
	if err != nil {
		// CI always has promtool — check-prom-rules is a hard gate and shells out
		// to it — so this skip only ever fires on a dev box without it.
		t.Skip("promtool not on PATH; the check-prom-rules gate covers CI")
	}

	crd, err := os.ReadFile(reconcilerRuleCRD)
	if err != nil {
		t.Fatalf("read PrometheusRule: %v", err)
	}
	// Run against the SHIPPED rules, extracted the same way the gate does — a
	// hand-copied duplicate would drift and prove nothing about production.
	bare, err := extractBareGroups(crd)
	if err != nil {
		t.Fatalf("extract spec.groups: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rules.yml"), bare, 0o644); err != nil {
		t.Fatal(err)
	}
	cases, err := os.ReadFile("testdata/promrules/reconciler_alerts_test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	testFile := filepath.Join(dir, "alerts_test.yml")
	if err := os.WriteFile(testFile, cases, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(promtool, "test", "rules", testFile).CombinedOutput()
	if err != nil {
		t.Fatalf("promtool test rules failed: %v\n%s", err, out)
	}
	t.Logf("promtool:\n%s", out)
}
