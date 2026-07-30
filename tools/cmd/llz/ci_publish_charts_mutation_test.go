package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// withPCSleepCounter swaps the retry sleep for a counter and restores it.
func withPCSleepCounter(t *testing.T) *int {
	t.Helper()
	n := 0
	prev := pcSleep
	pcSleep = func(time.Duration) { n++ }
	t.Cleanup(func() { pcSleep = prev })
	return &n
}

// TestRetryPCExhaustsExactlyTheBudget pins the retry arithmetic: --retries
// attempts, with a sleep BETWEEN attempts only, and a give-up at the budget.
// One attempt too few drops a publish that a single extra try would have landed
// (and the chart is then silently never published, since the version is already
// bumped); a sleep after the last attempt burns --interval seconds of CI for
// nothing; and a loop that keeps going until it happens to succeed hangs the
// merge-to-main job every cluster waits on for the new chart.
func TestRetryPCExhaustsExactlyTheBudget(t *testing.T) {
	sleeps := withPCSleepCounter(t)
	attempts := 0
	err := retryPC(publishChartsOpts{retries: 3, interval: time.Second}, "helm push llz-x", func() error {
		attempts++
		// Succeeds only far past the budget: a correct retryPC never gets here,
		// and a runaway one terminates (and fails) instead of hanging the suite.
		if attempts > 20 {
			return nil
		}
		return errors.New("registry 500")
	})
	if err == nil || !strings.Contains(err.Error(), "failed after 3 attempts") {
		t.Errorf("err = %v, want a give-up after 3 attempts", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (--retries), not retries-until-success", attempts)
	}
	if *sleeps != 2 {
		t.Errorf("sleeps = %d, want 2 (between attempts only, never after the last)", *sleeps)
	}
}

// TestRunPublishChartsSelectedFilter pins --selected in both directions. Publishing
// a chart the caller did not select is as wrong as skipping the one it did: the
// workflow dispatches a single chart by name when a targeted republish is needed.
func TestRunPublishChartsSelectedFilter(t *testing.T) {
	inspect := map[string][2]string{
		"cf": {"llz-cluster-foundation", "0.1.6"},
		"ob": {"llz-openbao-platform", "0.1.13"},
	}

	// "all" publishes every chart under --dir.
	rootAll := mkChartDirs(t, "cf", "ob")
	regAll := &fakeRegistry{published: map[string]bool{}, signed: map[string]bool{}}
	stubPublishSeams(t, regAll, inspect)
	captureStdout(t, func() {
		if err := runPublishCharts(publishChartsOpts{chartsDir: rootAll, selected: "all", registry: "ghcr.io", owner: "acme", repoPath: "charts", retries: 2}); err != nil {
			t.Fatalf("runPublishCharts(all): %v", err)
		}
	})
	if len(regAll.pushes) != 2 {
		t.Errorf("--selected=all pushed %v, want both charts", regAll.pushes)
	}

	// A named chart publishes ONLY that chart.
	rootOne := mkChartDirs(t, "cf", "ob")
	regOne := &fakeRegistry{published: map[string]bool{}, signed: map[string]bool{}}
	stubPublishSeams(t, regOne, inspect)
	captureStdout(t, func() {
		if err := runPublishCharts(publishChartsOpts{chartsDir: rootOne, selected: "llz-cluster-foundation", registry: "ghcr.io", owner: "acme", repoPath: "charts", retries: 2}); err != nil {
			t.Fatalf("runPublishCharts(selected): %v", err)
		}
	})
	if len(regOne.signs) != 1 || !strings.Contains(regOne.signs[0], "llz-cluster-foundation") {
		t.Errorf("--selected=llz-cluster-foundation signed %v, want only that chart", regOne.signs)
	}
}

// TestRunPublishChartsReportsCounts pins the summary tallies. They are the only
// record a publish run leaves of what it did — "Published 0" against a chart
// change is the signal that the version was not bumped, so a miscount hides
// exactly the immutability failure this command guards.
func TestRunPublishChartsReportsCounts(t *testing.T) {
	root := mkChartDirs(t, "cf", "ob", "ca")
	inspect := map[string][2]string{
		"cf": {"llz-cluster-foundation", "0.1.6"}, // published + signed → skipped
		"ob": {"llz-openbao-platform", "0.1.13"},  // published, UNSIGNED → re-signed
		"ca": {"llz-cert-automation", "0.1.5"},    // new → pushed
	}
	reg := &fakeRegistry{
		published: map[string]bool{
			"llz-cluster-foundation:0.1.6": true,
			"llz-openbao-platform:0.1.13":  true,
		},
		signed: map[string]bool{"ghcr.io/acme/charts/llz-cluster-foundation:0.1.6": true},
	}
	stubPublishSeams(t, reg, inspect)

	out := captureStdout(t, func() {
		if err := runPublishCharts(publishChartsOpts{chartsDir: root, selected: "all", registry: "ghcr.io", owner: "acme", repoPath: "charts", retries: 2}); err != nil {
			t.Fatalf("runPublishCharts: %v", err)
		}
	})
	const want = "Published 1 chart(s); re-signed 1 already-published unsigned chart(s)"
	if !strings.Contains(out, want) {
		t.Errorf("summary line missing %q:\n%s", want, out)
	}
}
