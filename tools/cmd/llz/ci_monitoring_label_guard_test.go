package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitMonitoringDocs(t *testing.T) {
	raw := `
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: good
  labels:
    prometheus: system
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: not-a-monitor
`
	docs := splitMonitoringDocs(raw)
	if len(docs) != 2 || docs[0].Kind != "ServiceMonitor" || docs[1].Kind != "Deployment" {
		t.Fatalf("expected SM + Deployment, got %+v", docs)
	}
	if docs[0].Metadata.Labels["prometheus"] != "system" {
		t.Errorf("label not parsed: %+v", docs[0].Metadata.Labels)
	}
}

func TestCollectMonitoringLabelFindings(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Compliant: has the label.
	write("ok-sm.yaml", "kind: ServiceMonitor\nmetadata:\n  name: ok\n  labels:\n    prometheus: system\n")
	// Violations: missing / wrong-valued label on selected kinds.
	write("bad-sm.yaml", "kind: ServiceMonitor\nmetadata:\n  name: bad-sm\n  labels:\n    app: x\n")
	write("bad-rule.yaml", "kind: PrometheusRule\nmetadata:\n  name: bad-rule\n")
	write("wrong-pm.yaml", "kind: PodMonitor\nmetadata:\n  name: wrong-pm\n  labels:\n    prometheus: other\n")
	// Not a monitoring kind → ignored even without the label.
	write("deploy.yaml", "kind: Deployment\nmetadata:\n  name: dep\n")

	findings, _, err := collectMonitoringLabelFindings([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range findings {
		got[f.name] = true
	}
	want := []string{"bad-sm", "bad-rule", "wrong-pm"}
	if len(findings) != len(want) {
		t.Fatalf("expected %d findings %v, got %d: %+v", len(want), want, len(findings), findings)
	}
	for _, n := range want {
		if !got[n] {
			t.Errorf("expected a finding for %q", n)
		}
	}
	if got["ok"] || got["dep"] {
		t.Error("compliant SM / non-monitoring kind must not be flagged")
	}
}

// A missing root (e.g. rendered/ not built) is skipped, not an error.
// TestMonitoringGuardEmptyCorpusFails inverts what this test used to assert. It
// required a missing root to be "skipped" — exit 0 — which is precisely how this
// guard could report green having examined nothing. Its whole reason for existing
// (the openbao ServiceMonitor renders `prometheus: system` from
// serviceMonitor.selectorLabels, so only the RENDERED tree shows the true value)
// lives under rendered/, and an unbuilt rendered/ IS the missing-root case.
func TestMonitoringGuardEmptyCorpusFails(t *testing.T) {
	err := runMonitoringLabelGuard([]string{filepath.Join(t.TempDir(), "does-not-exist")})
	if err == nil {
		t.Fatal("an empty corpus must FAIL — a guard with nothing to check reports the same green as one that checked everything")
	}
	if !strings.Contains(err.Error(), "examined 0") {
		t.Errorf("error should say it examined nothing: %v", err)
	}
}

// A root that EXISTS but holds no monitoring CRs is still a real corpus — the
// guard read files and found no violations. That must stay a pass, or every
// tree without a ServiceMonitor would fail.
func TestMonitoringGuardRealCorpusWithNoFindingsPasses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cm.yaml"),
		[]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runMonitoringLabelGuard([]string{dir}); err != nil {
		t.Errorf("a non-empty corpus with no violations must pass, got %v", err)
	}
}
