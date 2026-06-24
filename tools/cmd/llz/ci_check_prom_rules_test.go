package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const promRuleCRDValid = `apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: llz-rules
  labels:
    release: kube-prometheus-stack
spec:
  groups:
    - name: llz.rules
      rules:
        - alert: HighErrorRate
          expr: rate(http_requests_total{code="500"}[5m]) > 0.1
          for: 10m
          labels:
            severity: page
`

func TestExtractBareGroups(t *testing.T) {
	bare, err := extractBareGroups([]byte(promRuleCRDValid))
	if err != nil {
		t.Fatalf("extractBareGroups: %v", err)
	}
	got := string(bare)
	// The CRD wrapper (kind/metadata/spec) is gone; the document now starts at
	// the bare `groups:` key promtool expects, with the rule content preserved.
	if !strings.HasPrefix(strings.TrimSpace(got), "groups:") {
		t.Errorf("bare doc does not start with groups:\n%s", got)
	}
	for _, want := range []string{"name: llz.rules", "alert: HighErrorRate", "severity: page"} {
		if !strings.Contains(got, want) {
			t.Errorf("bare doc missing %q\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"kind:", "metadata:", "PrometheusRule"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("bare doc should not contain CRD wrapper %q\n%s", unwanted, got)
		}
	}
}

func TestExtractBareGroupsErrors(t *testing.T) {
	cases := []struct {
		name, in, wantErr string
	}{
		{"not a prometheusrule", "kind: ConfigMap\nspec:\n  groups: []\n", "not a PrometheusRule CRD (kind=ConfigMap)"},
		{"missing kind", "spec:\n  groups:\n    - name: x\n", "not a PrometheusRule CRD (kind=<none>)"},
		{"no groups key", "kind: PrometheusRule\nspec: {}\n", "has no spec.groups"},
		{"empty groups", "kind: PrometheusRule\nspec:\n  groups: []\n", "has no spec.groups"},
		{"null groups", "kind: PrometheusRule\nspec:\n  groups:\n", "has no spec.groups"},
		{"malformed yaml", "kind: PrometheusRule\nspec: [unterminated\n", "failed to parse YAML"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := extractBareGroups([]byte(tc.in))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// stubPromtool swaps promtoolCheckRules for the duration of a test, recording
// the bare-groups files it was handed.
func stubPromtool(t *testing.T, fail bool) *[]string {
	t.Helper()
	var seen []string
	orig := promtoolCheckRules
	promtoolCheckRules = func(path string) error {
		seen = append(seen, path)
		if fail {
			return &exitErr{}
		}
		// Sanity: promtool receives the bare-groups form, not the CRD wrapper.
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), "kind: PrometheusRule") {
			t.Errorf("promtool handed the CRD wrapper, not bare groups:\n%s", data)
		}
		return nil
	}
	t.Cleanup(func() { promtoolCheckRules = orig })
	return &seen
}

type exitErr struct{}

func (*exitErr) Error() string { return "exit status 1" }

func TestRunCICheckPromRulesPass(t *testing.T) {
	dir := t.TempDir()
	rule := filepath.Join(dir, "rules.yaml")
	if err := os.WriteFile(rule, []byte(promRuleCRDValid), 0o644); err != nil {
		t.Fatal(err)
	}
	seen := stubPromtool(t, false)

	var out bytes.Buffer
	if err := runCICheckPromRules(dir, nil, &out); err != nil {
		t.Fatalf("expected pass, got %v\n%s", err, out.String())
	}
	if len(*seen) != 1 || (*seen)[0] == "" {
		t.Errorf("promtool should run once on a tempfile, got %v", *seen)
	}
	if !strings.Contains(out.String(), "ok: "+rule) {
		t.Errorf("missing ok line:\n%s", out.String())
	}
}

func TestRunCICheckPromRulesPromtoolFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rules.yaml"), []byte(promRuleCRDValid), 0o644); err != nil {
		t.Fatal(err)
	}
	stubPromtool(t, true)

	var out bytes.Buffer
	err := runCICheckPromRules(dir, nil, &out)
	if err == nil {
		t.Fatal("expected failure when promtool rejects rules")
	}
	if !strings.Contains(err.Error(), "1 PrometheusRule file(s) failed validation") {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "::error file=") || !strings.Contains(out.String(), "promtool rejected rules") {
		t.Errorf("missing GHA error annotation:\n%s", out.String())
	}
}

func TestRunCICheckPromRulesBadCRD(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("kind: ConfigMap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seen := stubPromtool(t, false)

	var out bytes.Buffer
	err := runCICheckPromRules(dir, nil, &out)
	if err == nil {
		t.Fatal("expected failure for a non-PrometheusRule file")
	}
	if len(*seen) != 0 {
		t.Errorf("promtool must not run on an invalid CRD, ran on %v", *seen)
	}
	if !strings.Contains(out.String(), "not a PrometheusRule CRD") {
		t.Errorf("missing kind error:\n%s", out.String())
	}
}

func TestRunCICheckPromRulesSkipsMissingDir(t *testing.T) {
	seen := stubPromtool(t, false)
	var out bytes.Buffer
	if err := runCICheckPromRules(filepath.Join(t.TempDir(), "nope"), nil, &out); err != nil {
		t.Fatalf("absent rules dir should skip cleanly, got %v", err)
	}
	if len(*seen) != 0 {
		t.Errorf("promtool must not run when the dir is absent")
	}
	if !strings.Contains(out.String(), "skipping") {
		t.Errorf("expected a skip message:\n%s", out.String())
	}
}

func TestRunCICheckPromRulesExplicitArgs(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "explicit.yaml")
	if err := os.WriteFile(f, []byte(promRuleCRDValid), 0o644); err != nil {
		t.Fatal(err)
	}
	seen := stubPromtool(t, false)

	var out bytes.Buffer
	// Explicit args bypass --rules-dir entirely (the dir arg is a bogus path).
	if err := runCICheckPromRules("/does/not/exist", []string{f}, &out); err != nil {
		t.Fatalf("expected pass on explicit file, got %v", err)
	}
	if len(*seen) != 1 {
		t.Errorf("promtool should run on the one explicit file, got %v", *seen)
	}
}
