package main

// ci_assert_reconciler.go implements `llz ci assert-reconciler` — an e2e gate on
// the reconciler's FUNCTIONAL health, not just its pod phase.
//
// converge/health see the reconciler Deployment as green the moment its pod is
// Running+Ready — but the reconcile loop can be up yet silently failing: after
// the least-privilege RBAC tightening + CronJob retirement, a dropped permission
// (or lost OpenBao/Linode access) leaves the pod healthy while its samples report
// failure. The reconciler encodes exactly that as `llz_reconcile_up == 0`
// (reporting-but-failing) and `llz_reconcile_leader == 0` (no driving leader) —
// the LLZReconcilerReportingDown / LLZReconcilerNoLeader alerts.
//
// `alert-eval --strict` does NOT catch this: those alerts would be FIRING, and
// --strict only fails on DEAD?/BROKEN (a firing alert on a converging cluster is
// expected, so it can't gate). This asserts the gauge VALUES directly, so a
// reporting/leader failure reds the e2e instead of merely firing an alert nobody
// is paged by yet.
//
// Runs after assert-scrape-targets (which confirms the reconciler target is up +
// scraped), so the gauge here is fresh: any bad value is a real functional fault,
// not a first-scrape race. Reaches Prometheus via the shared ephemeral
// port-forward (prom_query.go). Read-only.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

func ciAssertReconcilerCmd() *cobra.Command {
	var prom, namespace string
	var settle, interval int
	c := &cobra.Command{
		Use:   "assert-reconciler",
		Short: "fail unless the reconciler is reporting healthy (llz_reconcile_up=1) and has a leader (llz_reconcile_leader=1)",
		Long: "Asserts the reconciler's functional gauges in the in-cluster Prometheus:\n" +
			"llz_reconcile_up == 1 (the reconcile loop is up AND its samples succeed — a\n" +
			"pod that is Running yet failing on lost RBAC/OpenBao access reports 0) and\n" +
			"llz_reconcile_leader == 1 (a replica holds the driving Lease). Catches the\n" +
			"silently-broken-reconciler class that converge/health (pod phase) and\n" +
			"alert-eval --strict (ignores FIRING) both miss. Polls a short settle budget,\n" +
			"then exits 0 (healthy) or 1. Read-only; ephemeral kubectl port-forward.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			os.Exit(runCIAssertReconciler(prom, namespace,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second))
			return nil
		},
	}
	c.Flags().StringVar(&prom, "prom", "monitoring/prometheus-operated:9090",
		"the Prometheus Service as <namespace>/<name>:<port> to port-forward to")
	c.Flags().StringVar(&namespace, "namespace", "llz-reconciler", "namespace label the reconciler gauges carry")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling for the reconciler to report healthy before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}

// promScalar parses an instant-query response, returning the first sample's value
// and whether ANY series was returned. A non-success status or unparseable body
// is treated as no series (the caller decides how to classify an absent gauge).
func promScalar(raw []byte) (value float64, hasSeries bool) {
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &resp) != nil || resp.Status != "success" {
		return 0, false
	}
	if len(resp.Data.Result) == 0 {
		return 0, false
	}
	v := resp.Data.Result[0].Value
	if len(v) != 2 {
		return 0, false
	}
	s, ok := v[1].(string)
	if !ok {
		return 0, false
	}
	f, perr := strconv.ParseFloat(s, 64)
	if perr != nil {
		return 0, false
	}
	return f, true
}

// gaugeCheck is one reconciler gauge assertion and its evaluated outcome.
type gaugeCheck struct {
	name    string  // the metric/label the check reads (for messages)
	query   string  // the PromQL instant query
	value   float64 // evaluated value (valid only when present)
	present bool    // a series was returned
	failWhy string  // human-readable failure reason (empty when OK)
}

// evalReconcilerGauge classifies a scalar gauge that must equal `want`. An absent
// series is a failure (the reconciler isn't emitting it — down or not reporting).
func evalReconcilerGauge(name, query string, raw []byte, want float64, absentWhy, mismatchWhy string) gaugeCheck {
	g := gaugeCheck{name: name, query: query}
	g.value, g.present = promScalar(raw)
	switch {
	case !g.present:
		g.failWhy = absentWhy
	case g.value != want:
		g.failWhy = mismatchWhy
	}
	return g
}

// reconcilerProbe is one poll's evaluation of the reconciler gauges.
type reconcilerProbe struct {
	up     gaugeCheck
	leader gaugeCheck
}

func (p reconcilerProbe) healthy() bool {
	return p.up.failWhy == "" && p.leader.failWhy == ""
}

// probeReconciler opens one port-forward and evaluates llz_reconcile_up +
// llz_reconcile_leader. A transport error is returned so the poll loop can retry.
func probeReconciler(prom, namespace string) (reconcilerProbe, error) {
	upQ := fmt.Sprintf(`max(llz_reconcile_up{namespace=%q})`, namespace)
	leaderQ := fmt.Sprintf(`max(llz_reconcile_leader{namespace=%q})`, namespace)
	var p reconcilerProbe
	err := withPrometheus(prom, func(get func(string) ([]byte, error)) error {
		upRaw, uerr := get("/api/v1/query?query=" + url.QueryEscape(upQ))
		if uerr != nil {
			return uerr
		}
		leaderRaw, lerr := get("/api/v1/query?query=" + url.QueryEscape(leaderQ))
		if lerr != nil {
			return lerr
		}
		p.up = evalReconcilerGauge("llz_reconcile_up", upQ, upRaw, 1,
			"no llz_reconcile_up series — the reconciler isn't reporting (pod down / not scraped)",
			"llz_reconcile_up=0 — the reconcile loop is up but its samples are failing (lost API/OpenBao access; check RBAC)")
		p.leader = evalReconcilerGauge("llz_reconcile_leader", leaderQ, leaderRaw, 1,
			"no llz_reconcile_leader series — the reconciler isn't reporting (pod down / not scraped)",
			"llz_reconcile_leader=0 — no replica holds the driving Lease (leader election stuck)")
		return nil
	})
	return p, err
}

func runCIAssertReconciler(prom, namespace string, settle, interval time.Duration) int {
	fmt.Println("## Reconciler functional-health assertion")

	var last reconcilerProbe
	var lastErr error
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		p, err := probeReconciler(prom, namespace)
		last, lastErr = p, err
		if err == nil && p.healthy() {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		if err != nil {
			fmt.Printf("attempt %d: could not reach Prometheus at %s (%v) — retrying in %s\n", attempt, prom, err, interval)
		} else {
			fmt.Printf("attempt %d: reconciler not healthy yet — retrying in %s\n", attempt, interval)
		}
		time.Sleep(interval)
	}

	if lastErr != nil {
		fmt.Fprintf(os.Stderr, "::error::could not reach Prometheus at %s within %s (%v)\n", prom, settle, lastErr)
		return 1
	}

	fail := false
	for _, g := range []gaugeCheck{last.up, last.leader} {
		if g.failWhy == "" {
			fmt.Printf("OK: %s=%g\n", g.name, g.value)
		} else {
			fmt.Printf("FAIL: %s\n", g.failWhy)
			fail = true
		}
	}
	if fail {
		fmt.Fprintln(os.Stderr, "::error::reconciler is not functionally healthy")
		return 1
	}
	fmt.Println("Reconciler is reporting healthy and has a leader.")
	return 0
}
