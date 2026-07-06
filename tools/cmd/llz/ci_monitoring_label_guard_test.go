package main

import (
	"os"
	"path/filepath"
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

	findings, err := collectMonitoringLabelFindings([]string{dir})
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
func TestMonitoringGuardMissingRootSkipped(t *testing.T) {
	if err := runMonitoringLabelGuard([]string{filepath.Join(t.TempDir(), "does-not-exist")}); err != nil {
		t.Errorf("missing root should be skipped, got %v", err)
	}
}
