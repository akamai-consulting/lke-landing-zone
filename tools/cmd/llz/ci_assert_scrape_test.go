package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseActiveTargets(t *testing.T) {
	raw := []byte(`{"status":"success","data":{"activeTargets":[
	  {"scrapePool":"serviceMonitor/llz-reconciler/llz-reconciler/0","health":"up","lastError":""},
	  {"scrapePool":"serviceMonitor/llz-openbao/platform-openbao/0","health":"down","lastError":"connection refused"},
	  {"scrapePool":"serviceMonitor/monitoring/apl-core-kube-state/0","health":"up","lastError":""}
	]}}`)
	got := parseActiveTargets(raw)
	if len(got) != 3 {
		t.Fatalf("expected 3 active targets, got %d", len(got))
	}
	if got[1].Health != "down" || got[1].LastError != "connection refused" {
		t.Errorf("down target not parsed: %+v", got[1])
	}
}

func TestLoadedRuleGroups(t *testing.T) {
	raw := []byte(`{"data":{"groups":[
	  {"name":"support-plane-alerts","rules":[]},
	  {"name":"openbao-alerts","rules":[]}
	]}}`)
	got := loadedRuleGroups(raw)
	if len(got) != 2 || got[0] != "support-plane-alerts" || got[1] != "openbao-alerts" {
		t.Fatalf("unexpected groups: %v", got)
	}
}

func TestEvalScrapeTargets(t *testing.T) {
	targets := []activeTarget{
		{ScrapePool: "serviceMonitor/llz-reconciler/llz-reconciler/0", Health: "up"},
		{ScrapePool: "serviceMonitor/llz-openbao/platform-openbao/0", Health: "down", LastError: "x509"},
		// a look-alike that must NOT match "llz-reconciler" (trailing-slash guard):
		{ScrapePool: "serviceMonitor/llz-reconciler/llz-reconciler-extra/0", Health: "up"},
	}
	expected := []string{
		"llz-reconciler/llz-reconciler",
		"llz-openbao/platform-openbao",
		"llz-observability/otel-collector-monitoring", // no target at all
	}
	got := evalScrapeTargets(expected, targets)
	if len(got) != 3 {
		t.Fatalf("expected 3 verdicts, got %d", len(got))
	}
	byName := map[string]monitorVerdict{}
	for _, v := range got {
		byName[v.Monitor] = v
	}

	if v := byName["llz-reconciler/llz-reconciler"]; v.Targets != 1 || v.Up != 1 || !v.OK() {
		t.Errorf("reconciler should have exactly 1 up target (no cross-match to -extra): %+v", v)
	}
	if v := byName["llz-openbao/platform-openbao"]; v.Targets != 1 || v.Up != 0 || v.OK() || v.LastErr != "x509" {
		t.Errorf("openbao should be discovered-but-down with lastError: %+v", v)
	}
	if v := byName["llz-observability/otel-collector-monitoring"]; v.Targets != 0 || v.OK() {
		t.Errorf("otel monitor should have 0 targets (never discovered): %+v", v)
	}
}

func TestMissingRuleGroups(t *testing.T) {
	expected := []string{"support-plane-alerts", "openbao-alerts", "llz-reconciler"}
	loaded := []string{"openbao-alerts", "some-apl-core-group"}
	missing := missingRuleGroups(expected, loaded)
	if len(missing) != 2 {
		t.Fatalf("expected 2 missing, got %v", missing)
	}
	if !contains(missing, "support-plane-alerts") || !contains(missing, "llz-reconciler") {
		t.Errorf("wrong missing set: %v", missing)
	}
	if len(missingRuleGroups(expected, []string{"support-plane-alerts", "openbao-alerts", "llz-reconciler"})) != 0 {
		t.Error("all-loaded should yield no missing")
	}
}

func TestScrapeProbeAllWired(t *testing.T) {
	up := monitorVerdict{Monitor: "a/b", Targets: 1, Up: 1}
	down := monitorVerdict{Monitor: "c/d", Targets: 1, Up: 0}
	if !(scrapeProbe{monitors: []monitorVerdict{up}, missing: nil}).allWired() {
		t.Error("one up monitor + no missing groups should be wired")
	}
	if (scrapeProbe{monitors: []monitorVerdict{up, down}}).allWired() {
		t.Error("a down monitor must fail allWired")
	}
	if (scrapeProbe{monitors: []monitorVerdict{up}, missing: []string{"g"}}).allWired() {
		t.Error("a missing rule group must fail allWired")
	}
}

func TestSplitCSVList(t *testing.T) {
	got := splitCSVList(" a/b , , c/d ,")
	if len(got) != 2 || got[0] != "a/b" || got[1] != "c/d" {
		t.Fatalf("splitCSVList mishandled trimming/empties: %v", got)
	}
	if len(splitCSVList("")) != 0 {
		t.Error("empty string should yield no entries")
	}
}

// probeScrapeState + the poll loop are seamed through withPrometheus; verify a
// transport error surfaces (retryable) and a wired cluster resolves the probe.
func TestProbeScrapeState(t *testing.T) {
	orig := withPrometheus
	t.Cleanup(func() { withPrometheus = orig })

	withPrometheus = func(_ string, _ func(func(string) ([]byte, error)) error) error {
		return errors.New("port-forward failed")
	}
	if _, err := probeScrapeState("ns/svc:9090", []string{"a/b"}, []string{"g"}); err == nil {
		t.Error("transport error should propagate for retry")
	}

	withPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(path string) ([]byte, error) {
			if strings.HasPrefix(path, "/api/v1/targets") {
				return []byte(`{"data":{"activeTargets":[{"scrapePool":"serviceMonitor/n/m/0","health":"up"}]}}`), nil
			}
			return []byte(`{"data":{"groups":[{"name":"g","rules":[]}]}}`), nil
		})
	}
	p, err := probeScrapeState("ns/svc:9090", []string{"n/m"}, []string{"g"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !p.allWired() {
		t.Errorf("expected wired probe, got %+v", p)
	}
}

// A zero settle budget must still probe once and (here) fail closed on a
// never-discovered monitor rather than hang.
func TestRunAssertScrapeFailsClosedFast(t *testing.T) {
	orig := withPrometheus
	t.Cleanup(func() { withPrometheus = orig })
	withPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(path string) ([]byte, error) {
			if strings.HasPrefix(path, "/api/v1/targets") {
				return []byte(`{"data":{"activeTargets":[]}}`), nil // nothing discovered
			}
			return []byte(`{"data":{"groups":[]}}`), nil
		})
	}
	if code := runCIAssertScrapeTargets("ns/svc:9090", []string{"a/b"}, nil, 0, time.Second); code != 1 {
		t.Errorf("expected exit 1 on a never-discovered monitor, got %d", code)
	}
}

// A fully wired cluster passes on the first probe (no retry, exit 0).
func TestRunAssertScrapePassesWhenWired(t *testing.T) {
	orig := withPrometheus
	t.Cleanup(func() { withPrometheus = orig })
	withPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(path string) ([]byte, error) {
			if strings.HasPrefix(path, "/api/v1/targets") {
				return []byte(`{"data":{"activeTargets":[{"scrapePool":"serviceMonitor/n/m/0","health":"up"}]}}`), nil
			}
			return []byte(`{"data":{"groups":[{"name":"g","rules":[]}]}}`), nil
		})
	}
	if code := runCIAssertScrapeTargets("ns/svc:9090", []string{"n/m"}, []string{"g"}, 30*time.Second, time.Second); code != 0 {
		t.Errorf("expected exit 0 on a wired cluster, got %d", code)
	}
}

// Exercises both remaining FAIL arms in one probe: a discovered-but-down monitor
// and a missing rule group. Zero settle so it fails closed on the first probe.
func TestRunAssertScrapeReportsDownAndMissing(t *testing.T) {
	orig := withPrometheus
	t.Cleanup(func() { withPrometheus = orig })
	withPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(path string) ([]byte, error) {
			if strings.HasPrefix(path, "/api/v1/targets") {
				return []byte(`{"data":{"activeTargets":[{"scrapePool":"serviceMonitor/n/m/0","health":"down","lastError":"x509"}]}}`), nil
			}
			return []byte(`{"data":{"groups":[]}}`), nil // group "g" absent
		})
	}
	if code := runCIAssertScrapeTargets("ns/svc:9090", []string{"n/m"}, []string{"g"}, 0, time.Second); code != 1 {
		t.Errorf("expected exit 1 on down target + missing group, got %d", code)
	}
}

// A transport error that never clears within the settle budget exits 1.
func TestRunAssertScrapeFailsOnUnreachable(t *testing.T) {
	orig := withPrometheus
	t.Cleanup(func() { withPrometheus = orig })
	withPrometheus = func(_ string, _ func(func(string) ([]byte, error)) error) error {
		return errors.New("port-forward failed")
	}
	if code := runCIAssertScrapeTargets("ns/svc:9090", []string{"n/m"}, nil, 0, time.Second); code != 1 {
		t.Errorf("expected exit 1 when Prometheus is unreachable, got %d", code)
	}
}
