package main

// ci_assert_scrape.go implements `llz ci assert-scrape-targets` — the gating
// counterpart to the report-only `alert-eval` / `prom-metrics` diagnostics. It
// asserts the observability pipeline is actually WIRED, not merely present:
//
//   1. Every landing-zone ServiceMonitor has a live, `up` scrape target in the
//      in-cluster Prometheus (via /api/v1/targets).
//   2. Every landing-zone PrometheusRule group is LOADED into Prometheus (via
//      /api/v1/rules).
//
// Why a dedicated gate. `converge`, `health`, and `assert-loki` all stay green
// even when metrics silently stop flowing — the exact failure mode of the
// ServiceMonitor/PrometheusRule label regressions (a missing `prometheus: system`
// label, a renamed Service port, a wrong namespaceSelector): the CRs exist, but
// apl-core's Prometheus never discovers/loads them, so every alert becomes a
// promtool-valid rule that can never fire. A report-only check cannot catch that;
// this fails the e2e.
//
// The expected sets are the KNOWN landing-zone monitors/groups, NOT derived from
// the cluster by the `prometheus: system` label. That is deliberate: if the label
// regresses at the source, a label-filtered `kubectl get` would return an empty
// expected set and the gate would pass green on the very bug it exists to catch.
// A monitor whose label is dropped produces no scrapePool in Prometheus, so the
// "0 active targets → FAIL" arm catches it independent of how the CR is labelled.
// Instances that ship a different surface override --monitors / --rule-groups.
//
// Reaches Prometheus via the shared ephemeral port-forward (prom_query.go — the
// apiserver Service proxy is webhook-denied on LKE-Enterprise), same as
// alert-eval. Read-only. Polls for a short settle budget so a first-scrape race
// on a freshly converged cluster doesn't flake the gate.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// defaultScrapeMonitors are the landing-zone ServiceMonitors apl-core's
// Prometheus must scrape (namespace/name). Kept in sync with the first-party
// ServiceMonitor manifests under instance-template/apl-values plus the rendered
// OpenBao chart output. Each must yield at least one `up` target.
var defaultScrapeMonitors = []string{
	"llz-observability/cert-manager",
	"llz-observability/otel-collector-monitoring",
	"llz-reconciler/llz-reconciler",
	"llz-openbao/platform-openbao",
}

// defaultScrapeRuleGroups are the PrometheusRule GROUP names (spec.groups[].name,
// not the CR name) apl-core's Prometheus must load. A group absent from
// /api/v1/rules means its PrometheusRule was never picked up (ruleSelector miss).
var defaultScrapeRuleGroups = []string{
	"credential-certs",
	"credential-tokens",
	"support-plane-alerts",
	"openbao-alerts",
	"llz-reconciler",
}

func ciAssertScrapeTargetsCmd() *cobra.Command {
	var prom, monitors, ruleGroups string
	var settle, interval int
	c := &cobra.Command{
		Use:   "assert-scrape-targets",
		Short: "fail unless every landing-zone ServiceMonitor is scraped (up) and every PrometheusRule group is loaded",
		Long: "Gates the observability pipeline: asserts the in-cluster Prometheus has a\n" +
			"live `up` target for each landing-zone ServiceMonitor (/api/v1/targets) AND has\n" +
			"loaded each landing-zone PrometheusRule group (/api/v1/rules). Catches the\n" +
			"label/port/selector regressions that leave the CRs present but silently\n" +
			"un-scraped/un-loaded — which converge/health/assert-loki all miss. Polls for a\n" +
			"short settle budget to absorb a first-scrape race, then exits 0 (all wired) or\n" +
			"1. Read-only; reaches Prometheus via an ephemeral kubectl port-forward.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertScrapeTargets(prom,
				splitCSVList(monitors), splitCSVList(ruleGroups),
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&prom, "prom", "monitoring/prometheus-operated:9090",
		"the Prometheus Service as <namespace>/<name>:<port> to port-forward to")
	c.Flags().StringVar(&monitors, "monitors", strings.Join(defaultScrapeMonitors, ","),
		"comma-separated ServiceMonitors (namespace/name) that must each have an `up` target")
	c.Flags().StringVar(&ruleGroups, "rule-groups", strings.Join(defaultScrapeRuleGroups, ","),
		"comma-separated PrometheusRule group names that must each be loaded")
	c.Flags().IntVar(&settle, "settle", 180, "seconds to keep polling for the pipeline to come up before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}

// splitCSVList splits a comma-separated flag into trimmed, non-empty entries.
func splitCSVList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// activeTarget is the subset of a /api/v1/targets active target the gate reads.
type activeTarget struct {
	ScrapePool string
	Health     string // "up" | "down" | "unknown"
	LastError  string
}

// parseActiveTargets extracts the active targets from a /api/v1/targets response.
func parseActiveTargets(raw []byte) []activeTarget {
	var resp struct {
		Data struct {
			ActiveTargets []struct {
				ScrapePool string `json:"scrapePool"`
				Health     string `json:"health"`
				LastError  string `json:"lastError"`
			} `json:"activeTargets"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return nil
	}
	out := make([]activeTarget, 0, len(resp.Data.ActiveTargets))
	for _, t := range resp.Data.ActiveTargets {
		out = append(out, activeTarget{ScrapePool: t.ScrapePool, Health: t.Health, LastError: t.LastError})
	}
	return out
}

// loadedRuleGroups extracts the loaded group names from a /api/v1/rules response.
func loadedRuleGroups(raw []byte) []string {
	var rules promRulesJSON // shared with ci_prom_rules.go
	if json.Unmarshal(raw, &rules) != nil {
		return nil
	}
	out := make([]string, 0, len(rules.Data.Groups))
	for _, g := range rules.Data.Groups {
		out = append(out, g.Name)
	}
	return out
}

// monitorVerdict is the per-ServiceMonitor scrape outcome.
type monitorVerdict struct {
	Monitor string // namespace/name
	Targets int    // active targets discovered in its scrapePool
	Up      int    // of those, healthy
	LastErr string // first non-empty lastError among the down targets (diagnostic)
}

// OK reports whether the monitor has at least one healthy target.
func (v monitorVerdict) OK() bool { return v.Targets > 0 && v.Up > 0 }

// evalScrapeTargets computes, for each expected monitor (namespace/name), how
// many active targets Prometheus discovered for it and how many are `up`. A
// prometheus-operator target's scrapePool is "serviceMonitor/<ns>/<name>/<idx>",
// so a monitor's targets are those whose scrapePool has the "serviceMonitor/
// <ns>/<name>/" prefix. Zero targets = never discovered (the label/selector
// regression); >0 but none up = discovered yet unreachable (NetworkPolicy / port
// / TLS). Pure.
func evalScrapeTargets(expected []string, targets []activeTarget) []monitorVerdict {
	out := make([]monitorVerdict, 0, len(expected))
	for _, m := range expected {
		prefix := "serviceMonitor/" + m + "/"
		v := monitorVerdict{Monitor: m}
		for _, t := range targets {
			if !strings.HasPrefix(t.ScrapePool, prefix) {
				continue
			}
			v.Targets++
			if t.Health == "up" {
				v.Up++
			} else if v.LastErr == "" && t.LastError != "" {
				v.LastErr = t.LastError
			}
		}
		out = append(out, v)
	}
	return out
}

// missingRuleGroups returns the expected group names not present in loaded.
func missingRuleGroups(expected, loaded []string) []string {
	have := map[string]bool{}
	for _, g := range loaded {
		have[g] = true
	}
	var missing []string
	for _, g := range expected {
		if !have[g] {
			missing = append(missing, g)
		}
	}
	return missing
}

// scrapeProbe is one poll's result: the per-monitor verdicts and the rule groups
// still missing. allWired reports whether the gate can pass now.
type scrapeProbe struct {
	monitors []monitorVerdict
	missing  []string
}

func (p scrapeProbe) allWired() bool {
	for _, v := range p.monitors {
		if !v.OK() {
			return false
		}
	}
	return len(p.missing) == 0
}

// probeScrapeState opens one port-forward, fetches targets + rules, and computes
// a scrapeProbe. A transport error is returned so the poll loop can retry.
func probeScrapeState(prom string, monitors, ruleGroups []string) (scrapeProbe, error) {
	var p scrapeProbe
	err := withPrometheus(prom, func(get func(string) ([]byte, error)) error {
		targetsJSON, terr := get("/api/v1/targets?state=active")
		if terr != nil {
			return terr
		}
		rulesJSON, rerr := get("/api/v1/rules")
		if rerr != nil {
			return rerr
		}
		p.monitors = evalScrapeTargets(monitors, parseActiveTargets(targetsJSON))
		p.missing = missingRuleGroups(ruleGroups, loadedRuleGroups(rulesJSON))
		return nil
	})
	return p, err
}

// runCIAssertScrapeTargets returns nil when the pipeline is fully wired and an
// error otherwise (cobra exits 1 on it). The ::error:: annotations stay as
// direct writes: GitHub parses an annotation only at the start of a line, and a
// returned error reaches stderr behind main.go's "llz: " prefix.
func runCIAssertScrapeTargets(prom string, monitors, ruleGroups []string, settle, interval time.Duration) error {
	sort.Strings(monitors)
	sort.Strings(ruleGroups)
	fmt.Println("## Scrape-target + rule-load assertion")
	if len(monitors) == 0 && len(ruleGroups) == 0 {
		fmt.Fprintln(os.Stderr, "::error::no --monitors and no --rule-groups to assert — refusing to pass vacuously")
		return fmt.Errorf("no --monitors and no --rule-groups to assert — refusing to pass vacuously")
	}

	var last scrapeProbe
	var lastErr error
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		p, err := probeScrapeState(prom, monitors, ruleGroups)
		last, lastErr = p, err
		if err == nil && p.allWired() {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		if err != nil {
			fmt.Printf("attempt %d: could not reach Prometheus at %s (%v) — retrying in %s\n", attempt, prom, err, interval)
		} else {
			fmt.Printf("attempt %d: pipeline not fully wired yet — retrying in %s\n", attempt, interval)
		}
		time.Sleep(interval)
	}

	if lastErr != nil {
		fmt.Fprintf(os.Stderr, "::error::could not reach Prometheus at %s within %s (%v)\n", prom, settle, lastErr)
		return fmt.Errorf("could not reach Prometheus at %s within %s: %w", prom, settle, lastErr)
	}

	fail := false
	for _, v := range last.monitors {
		switch {
		case v.OK():
			fmt.Printf("OK: ServiceMonitor %s (%d/%d targets up)\n", v.Monitor, v.Up, v.Targets)
		case v.Targets == 0:
			fmt.Printf("FAIL: ServiceMonitor %s has NO scrape target — never discovered (missing `prometheus: system` label / selector / namespace mismatch)\n", v.Monitor)
			fail = true
		default:
			detail := ""
			if v.LastErr != "" {
				detail = " — lastError: " + v.LastErr
			}
			fmt.Printf("FAIL: ServiceMonitor %s discovered but 0/%d targets up (NetworkPolicy / port / TLS)%s\n", v.Monitor, v.Targets, detail)
			fail = true
		}
	}
	if len(last.missing) > 0 {
		fmt.Printf("FAIL: PrometheusRule group(s) not loaded into Prometheus: %s (ruleSelector miss — missing `prometheus: system` label?)\n", strings.Join(last.missing, ", "))
		fail = true
	} else if len(ruleGroups) > 0 {
		fmt.Printf("OK: %d PrometheusRule group(s) loaded\n", len(ruleGroups))
	}

	if fail {
		fmt.Fprintln(os.Stderr, "::error::observability scrape/rule pipeline is not fully wired")
		return fmt.Errorf("observability scrape/rule pipeline is not fully wired")
	}
	fmt.Println("Scrape targets are up and rule groups are loaded.")
	return nil
}
