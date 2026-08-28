package assertobs

// Support-plane alert SEMANTICS — does the rule fire when the thing it is named
// for happens?
//
// `check-prom-rules` proves the expressions parse, and it proved that every day
// while `LokiStatefulSetUnavailable` was incapable of firing: it selected
// `statefulset="loki"` on a cluster whose Loki StatefulSet is `loki-ingester`, and
// it tested `== 0` on a fleet whose failure mode was 1-of-3 ready. Sixteen days of
// log-ingestion outage, no alert, no error, a green syntax gate.
//
// Nothing static could have caught it. A guard comparing the selector against the
// workload names would need a live cluster to know them; a rule that parses is
// still a rule that matches nothing. What separates a live rule from a firing one
// is EXECUTING it against series named the way the cluster names them, which is
// what promtool's rule unit tests do.
//
// Run against the SHIPPED CRD, extracted the way the gate extracts it — a
// hand-copied rules file would drift and prove nothing about production.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// supportPlaneRuleCRD is the PrometheusRule under test, relative to this package.
const supportPlaneRuleCRD = "../../../../../platform-apl/components/observability/prometheus-rules/support-plane-alerts.yaml"

func TestSupportPlaneAlertSemantics(t *testing.T) {
	promtool, err := exec.LookPath("promtool")
	if err != nil {
		// CI always has promtool — check-prom-rules is a hard gate and shells out
		// to it — so this skip only ever fires on a dev box without it.
		t.Skip("promtool not on PATH; the check-prom-rules gate covers CI")
	}

	crd, err := os.ReadFile(supportPlaneRuleCRD)
	if err != nil {
		t.Fatalf("read PrometheusRule: %v", err)
	}
	bare, err := extractBareGroups(crd)
	if err != nil {
		t.Fatalf("extract spec.groups: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rules.yml"), bare, 0o644); err != nil {
		t.Fatal(err)
	}
	cases, err := os.ReadFile("testdata/promrules/support_plane_alerts_test.yaml")
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
