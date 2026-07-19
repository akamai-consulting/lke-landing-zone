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
	"strings"
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
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertReconciler(prom, namespace,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
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
// promScalar returns the first sample's value, whether a series came back, and
// whether the QUERY ITSELF failed.
//
// The third return is the point. This used to fold four different conditions
// into hasSeries=false — unparseable JSON, status!="success" (Prometheus
// returned an error), a genuinely absent series, and a malformed value tuple —
// and evalReconcilerGauge then reported all four with its absentWhy string,
// e.g. "the reconciler isn't reporting (pod down / not scraped)".
//
// So a Prometheus hiccup failed a GATING e2e assert while blaming the
// reconciler: the operator goes and inspects a pod that is perfectly healthy.
// Same shape as the OpenBao false-SEALED report and alert-eval's vacuous
// --strict pass — "could not ask" is not an answer about the subject.
func promScalar(raw []byte) (value float64, hasSeries bool, queryErr error) {
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, false, fmt.Errorf("unparseable Prometheus response: %w", err)
	}
	if resp.Status != "success" {
		detail := resp.Error
		if detail == "" {
			detail = "status=" + resp.Status
		}
		return 0, false, fmt.Errorf("Prometheus returned an error: %s", detail)
	}
	if len(resp.Data.Result) == 0 {
		return 0, false, nil // a real answer: the series genuinely is not there
	}
	v := resp.Data.Result[0].Value
	if len(v) != 2 {
		return 0, false, fmt.Errorf("malformed sample: expected [ts, value], got %d element(s)", len(v))
	}
	str, ok := v[1].(string)
	if !ok {
		return 0, false, fmt.Errorf("malformed sample: value is %T, not a string", v[1])
	}
	f, perr := strconv.ParseFloat(str, 64)
	if perr != nil {
		return 0, false, fmt.Errorf("malformed sample: value %q is not numeric", str)
	}
	return f, true, nil
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
	var qerr error
	g.value, g.present, qerr = promScalar(raw)
	switch {
	case qerr != nil:
		// The query did not answer. Do NOT reuse absentWhy — that blames the
		// reconciler for a Prometheus failure.
		g.failWhy = fmt.Sprintf("could not evaluate %q against Prometheus (%v) — this is a QUERY failure, not evidence about the reconciler", query, qerr)
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

// runCIAssertReconciler returns nil when the reconciler is functionally healthy
// and an error otherwise (cobra exits 1 on it). The ::error:: annotations stay
// as direct writes: GitHub parses an annotation only at the start of a line, and
// a returned error reaches stderr behind main.go's "llz: " prefix.
func runCIAssertReconciler(prom, namespace string, settle, interval time.Duration) error {
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
		return fmt.Errorf("could not reach Prometheus at %s within %s: %w", prom, settle, lastErr)
	}

	// up has no authoritative fallback — a failing sample loop is a real fault.
	upFail := last.up.failWhy != ""
	if upFail {
		fmt.Printf("FAIL: %s\n", last.up.failWhy)
	} else {
		fmt.Printf("OK: %s=%g\n", last.up.name, last.up.value)
	}

	// leader is a DERIVED signal (process gauge → 30s scrape → PromQL) and can lag
	// a real handoff. When it reads unhealthy but up is fine, cross-check the
	// AUTHORITATIVE Lease before failing: a held, fresh Lease means a leader
	// genuinely exists and the gauge merely lagged (pass); an absent, holderless,
	// or stale Lease is a real election stall (fail). This never masks a broken
	// reconciler — leaseLeaderFresh only reports live for a Lease the elector
	// itself would still consider held.
	leaderFail := last.leader.failWhy != ""
	switch {
	case !leaderFail:
		fmt.Printf("OK: %s=%g\n", last.leader.name, last.leader.value)
	case !upFail:
		if holder, live := reconcilerLeaseLive(namespace, time.Now()); live {
			fmt.Printf("OK (via Lease): %s — but the %s Lease is held by %q with a fresh renewTime, so a leader exists and the gauge lagged\n",
				last.leader.failWhy, reconcilerLeaseName, holder)
			leaderFail = false
		} else {
			fmt.Printf("FAIL: %s (and the %s Lease has no live holder)\n", last.leader.failWhy, reconcilerLeaseName)
		}
	default:
		fmt.Printf("FAIL: %s\n", last.leader.failWhy)
	}

	if upFail || leaderFail {
		dumpReconcilerDiagnostics(namespace)
		fmt.Fprintln(os.Stderr, "::error::reconciler is not functionally healthy")
		return fmt.Errorf("reconciler is not functionally healthy")
	}
	fmt.Println("Reconciler is reporting healthy and has a leader.")
	return nil
}

// reconcilerLeaseName is the leader-election Lease the elector maintains
// (newLeaderElector(…, "llz-reconciler-leader", …) in reconcile.go).
const reconcilerLeaseName = "llz-reconciler-leader"

// reconcilerLeaseMaxAge mirrors the elector's leaseDuration: a Lease whose
// renewTime is older than this is one the elector itself treats as expired, so it
// cannot be a live leader. Keep in step with newLeaderElector's leaseDuration.
const reconcilerLeaseMaxAge = 30 * time.Second

// leaseLeaderFresh parses a coordination.k8s.io/v1 Lease (kubectl -o json) and
// reports its holder and whether a live leader holds it — a non-empty
// holderIdentity whose renewTime is within maxAge of now. An unparseable,
// holderless, or stale Lease is NOT live, so this can only confirm a real leader,
// never invent one: it turns a gauge-lag false-negative into a pass but leaves a
// genuine no-leader stall failing.
func leaseLeaderFresh(raw []byte, now time.Time, maxAge time.Duration) (holder string, fresh bool) {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return "", false
	}
	holder, renew := leaseHolderRenew(obj)
	if holder == "" || renew.IsZero() {
		return holder, false
	}
	return holder, now.Sub(renew) <= maxAge
}

// reconcilerLeaseLive reads the leader-election Lease straight from the API — the
// authoritative source, unlike the scraped gauge — and reports whether a live
// leader holds it. A kubectl error (NotFound / no access) is treated as not-live,
// so a missing Lease fails closed. Swappable for tests.
var reconcilerLeaseLive = func(namespace string, now time.Time) (holder string, live bool) {
	out, err := execOutput("kubectl", "-n", namespace, "get", "lease", reconcilerLeaseName, "-o", "json")
	if err != nil {
		return "", false
	}
	return leaseLeaderFresh(out, now, reconcilerLeaseMaxAge)
}

// dumpReconcilerDiagnostics prints best-effort reconciler state to stderr when the
// assertion fails. Without it the gate says only WHAT is wrong ("leader=0") and
// never WHY — and the e2e cluster is torn down seconds later, so the evidence is
// gone. Each dump maps to a concrete failure mode of a persistent leader=0 with
// up=1 (the observed flake): RESTARTS>0 in `get pods` is a crashloop inside the
// settle window; the Lease's holderIdentity/renewTime say whether a replica
// actually holds+renews it (the gauge lagging a live Lease is a scrape race, an
// empty/stale Lease is a real election stall); the logs surface an OpenBao/Linode
// dependency error or a 403 on the leases Role; describe's Events show OOMKilled /
// scheduling. Read-only and error-tolerant — execCombined swallows exit status and
// surfaces the tool's own "NotFound"/"No resources found" text, so a missing
// object degrades to a one-line note instead of aborting the dump.
func dumpReconcilerDiagnostics(namespace string) {
	fmt.Fprintln(os.Stderr, "::group::reconciler diagnostics (assertion failed)")
	defer fmt.Fprintln(os.Stderr, "::endgroup::")

	dumps := []struct {
		label string
		args  []string
	}{
		{"pods — RESTARTS>0 = crashloop inside the settle window",
			[]string{"-n", namespace, "get", "pods", "-l", "app.kubernetes.io/name=llz-reconciler", "-o", "wide"}},
		{"leader Lease — holderIdentity + renewTime = who drives + how fresh (empty/stale = real stall; fresh = gauge scrape-lag)",
			[]string{"-n", namespace, "get", "lease", reconcilerLeaseName, "-o", "yaml"}},
		{"logs (current) — an OpenBao/Linode sample error or a leases-Role 403 shows here",
			[]string{"-n", namespace, "logs", "deploy/llz-reconciler", "--tail=100", "--all-containers", "--timestamps"}},
		{"logs (previous container — present only if it restarted)",
			[]string{"-n", namespace, "logs", "deploy/llz-reconciler", "--tail=50", "--all-containers", "--previous", "--timestamps"}},
		{"pod events — OOMKilled / scheduling / image pull",
			[]string{"-n", namespace, "describe", "pods", "-l", "app.kubernetes.io/name=llz-reconciler"}},
	}
	for _, d := range dumps {
		out := strings.TrimRight(execCombined("kubectl", d.args...), "\n")
		if out == "" {
			out = "(no output)"
		}
		fmt.Fprintf(os.Stderr, "== %s ==\n%s\n", d.label, out)
	}
}
