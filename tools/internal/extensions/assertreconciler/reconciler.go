package assertreconciler

// ci_assert_reconciler.go implements `llz ci assert-reconciler` — an e2e gate on
// the reconciler's FUNCTIONAL health, not just its pod phase.
//
// converge/health see the reconciler Deployment as color.Green the moment its pod is
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
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/promwire"
)

// ── per-lane freshness ───────────────────────────────────────────────────────

// alwaysOnReconcilerLane is the sampling lane that runs unconditionally
// (reconcile.go registers it before any --reconcile-* flag is consulted), so it
// is expected even when the Deployment declares no optional lanes at all.
const alwaysOnReconcilerLane = "observe"

// reconcileFlagLane maps a Deployment `--reconcile-<x>` argument to the lane name
// that argument switches on — i.e. the `reconciler` label its gauges carry.
//
// This table is the CONTRACT between reconcile.go's flag registration and its
// lane registration, and it is the split-contract archetype in miniature: the
// flag name and the lane name are chosen independently and have already drifted
// once (`--reconcile-cidr-firewall` drives the lane called `cidr-firewall`, but
// `--reconcile-openbao-gauges` drives `openbao-gauges` and
// `--reconcile-linode-creds` drives `linode-creds` — three different truncation
// habits). TestReconcileFlagLaneTableMatchesReconcileGo pins it against the real
// registrations so a renamed lane fails at PR time instead of silently dropping
// out of this gate's expected set.
var reconcileFlagLane = map[string]string{
	"--reconcile-argo-nudge":        "argo-nudge",
	"--reconcile-cidr-firewall":     "cidr-firewall",
	"--reconcile-volume-labels":     "volume-labels",
	"--reconcile-volume-tags":       "volume-tags",
	"--reconcile-sc-demote":         "sc-demote",
	"--reconcile-linode-creds":      "linode-creds",
	"--reconcile-es-store-recovery": "es-store-recovery",
	"--reconcile-openbao-gauges":    "openbao-gauges",
	"--reconcile-token-inventory":   "token-inventory",
	"--reconcile-apl-overlay":       "apl-overlay",
}

// lanesFromDeploymentArgs extracts the enabled lane names from a reconciler
// container's args. Pure.
//
// It accepts both `--reconcile-x` and `--reconcile-x=true`, and honours an
// explicit `=false` as OFF — otherwise a lane deliberately disabled by an
// instance would be demanded here and fail a perfectly healthy cluster. Unknown
// `--reconcile-*` args are IGNORED rather than guessed at: a new lane must be
// added to reconcileFlagLane (and its test) to be gated, which is a visible
// omission rather than a silent one.
func lanesFromDeploymentArgs(args []string) []string {
	out := []string{alwaysOnReconcilerLane}
	seen := map[string]bool{alwaysOnReconcilerLane: true}
	for _, a := range args {
		flag, val := a, ""
		if i := strings.IndexByte(a, '='); i >= 0 {
			flag, val = a[:i], a[i+1:]
		}
		lane, known := reconcileFlagLane[flag]
		if !known || seen[lane] {
			continue
		}
		if val == "false" || val == "0" {
			continue
		}
		seen[lane] = true
		out = append(out, lane)
	}
	sort.Strings(out)
	return out
}

// enabledReconcilerLanes reads the shipped Deployment and returns the lanes it
// switches on. Swappable for tests. A kubectl failure returns an error rather
// than an empty set: an empty expected set would make this gate pass having
// demanded nothing, which is the vacuous pass the whole battery refuses.
var enabledReconcilerLanes = func(namespace string) ([]string, error) {
	out, err := deps.Cluster.Run("-n", namespace, "get", "deploy", "llz-reconciler",
		"-o", "jsonpath={.spec.template.spec.containers[*].args}")
	if err != nil {
		return nil, fmt.Errorf("reading llz-reconciler Deployment args: %w", err)
	}
	var args []string
	if err := json.Unmarshal(out, &args); err != nil {
		// jsonpath over a multi-container pod concatenates arrays; fall back to a
		// whitespace split, which is enough to spot the --reconcile-* tokens.
		args = strings.Fields(strings.NewReplacer("[", " ", "]", " ", "\"", " ", ",", " ").Replace(string(out)))
	}
	return lanesFromDeploymentArgs(args), nil
}

// laneVerdict is one lane's freshness outcome.
type laneVerdict struct {
	Lane    string
	Age     time.Duration // since its last successful pass (valid only when Present)
	Budget  time.Duration // staleFactor × the lane's own interval
	Present bool          // a last-success series exists for this lane
	FailWhy string        // empty when OK
}

// evalLaneFreshness decides, for each EXPECTED lane, whether it has passed
// recently enough. Pure.
//
// Each lane is judged against its OWN cadence: apl-overlay runs every 300s and
// volume-labels has a 3600s resync floor, so one global budget would either
// excuse a dead fast lane or condemn a healthy slow one. That is the same
// per-lane join the LLZReconcilerStale rule does; this reuses the reasoning
// rather than inventing a second definition of "stale".
//
// A missing interval sample falls back to defaultInterval — the lane is reporting
// successes, so it is alive, and refusing to judge it would silently drop it from
// the gate.
func evalLaneFreshness(expected []string, lastSuccess, intervals map[string]float64,
	now time.Time, staleFactor int, defaultInterval time.Duration) []laneVerdict {

	if staleFactor < 1 {
		staleFactor = 1
	}
	out := make([]laneVerdict, 0, len(expected))
	for _, lane := range expected {
		v := laneVerdict{Lane: lane}
		ts, ok := lastSuccess[lane]
		if !ok {
			v.FailWhy = "no llz_reconcile_last_success_timestamp_seconds series — the lane is enabled in the Deployment but has never reported a successful pass (it never started, or every pass has failed since boot)"
			out = append(out, v)
			continue
		}
		v.Present = true
		iv := defaultInterval
		if s, ok := intervals[lane]; ok && s > 0 {
			iv = time.Duration(s) * time.Second
		}
		v.Budget = time.Duration(staleFactor) * iv
		v.Age = now.Sub(time.Unix(int64(ts), 0))
		if v.Age > v.Budget {
			v.FailWhy = fmt.Sprintf("last successful pass was %s ago, over its %s budget (%d × %s cadence) — the lane is wedged or erroring every pass",
				v.Age.Round(time.Second), v.Budget, staleFactor, iv)
		}
		out = append(out, v)
	}
	return out
}

// defaultLaneInterval is the fallback cadence for a lane reporting successes but
// no interval sample. The slowest shipped resync floor (3600s) — generous on
// purpose, so the fallback can only ever under-report staleness, never invent it.
const defaultLaneInterval = 3600 * time.Second

// probeReconcilerLanes evaluates per-lane freshness over an already-open getter.
func probeReconcilerLanes(get func(string) ([]byte, error), namespace string,
	expected []string, staleFactor int, now time.Time) ([]laneVerdict, error) {

	successQ := fmt.Sprintf(`llz_reconcile_last_success_timestamp_seconds{namespace=%q}`, namespace)
	intervalQ := fmt.Sprintf(`llz_reconcile_interval_seconds{namespace=%q}`, namespace)
	sRaw, err := get("/api/v1/query?query=" + url.QueryEscape(successQ))
	if err != nil {
		return nil, err
	}
	iRaw, err := get("/api/v1/query?query=" + url.QueryEscape(intervalQ))
	if err != nil {
		return nil, err
	}
	success, err := promwire.VectorByLabel(sRaw, "reconciler")
	if err != nil {
		return nil, err
	}
	// An unreadable interval vector is NOT fatal: the fallback cadence keeps the
	// freshness question answerable, and failing here would blame the lanes for a
	// second query's problem.
	intervals, _ := promwire.VectorByLabel(iRaw, "reconciler")
	return evalLaneFreshness(expected, success, intervals, now, staleFactor, defaultLaneInterval), nil
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
	g.value, g.present, qerr = promwire.Scalar(raw)
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
	lanes  []laneVerdict
}

// staleLanes returns the lanes that failed their freshness budget.
func (p reconcilerProbe) staleLanes() []laneVerdict {
	var out []laneVerdict
	for _, v := range p.lanes {
		if v.FailWhy != "" {
			out = append(out, v)
		}
	}
	return out
}

func (p reconcilerProbe) healthy() bool {
	return p.up.failWhy == "" && p.leader.failWhy == "" && len(p.staleLanes()) == 0
}

// probeReconciler opens one port-forward and evaluates llz_reconcile_up +
// llz_reconcile_leader, plus per-lane freshness when expectedLanes is non-empty.
// A transport error is returned so the poll loop can retry.
func probeReconciler(prom, namespace string, expectedLanes []string, staleFactor int) (reconcilerProbe, error) {
	upQ := fmt.Sprintf(`max(llz_reconcile_up{namespace=%q})`, namespace)
	leaderQ := fmt.Sprintf(`max(llz_reconcile_leader{namespace=%q})`, namespace)
	var p reconcilerProbe
	err := deps.WithPrometheus(prom, func(get func(string) ([]byte, error)) error {
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
		if len(expectedLanes) > 0 {
			lanes, lerr := probeReconcilerLanes(get, namespace, expectedLanes, staleFactor, time.Now())
			if lerr != nil {
				return lerr
			}
			p.lanes = lanes
		}
		return nil
	})
	return p, err
}

// runCIAssertReconciler returns nil when the reconciler is functionally healthy
// and an error otherwise (cobra exits 1 on it). The ::error:: annotations stay
// as direct writes: GitHub parses an annotation only at the start of a line, and
// a returned error reaches stderr behind main.go's "llz: " prefix.
func runCIAssertReconciler(prom, namespace string, checkLanes bool, laneSet []string, staleFactor int,
	settle, interval time.Duration) error {
	fmt.Println("## Reconciler functional-health assertion")

	// Resolve the expected lane set BEFORE polling. A failure to read the
	// Deployment is fatal rather than "check no lanes": silently degrading to an
	// empty expectation is how this gate would report success having demanded
	// nothing of the very lanes it exists to watch.
	var expected []string
	if checkLanes {
		switch {
		case len(laneSet) > 0:
			expected = laneSet
		default:
			var err error
			if expected, err = enabledReconcilerLanes(namespace); err != nil {
				fmt.Fprintf(os.Stderr, "::error::could not determine which reconciler lanes are enabled (%v)\n", err)
				return fmt.Errorf("could not determine which reconciler lanes are enabled: %w", err)
			}
		}
		fmt.Printf("expected lanes (from the Deployment's --reconcile-* args): %s\n", strings.Join(expected, ", "))
	}

	var last reconcilerProbe
	var lastErr error
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		p, err := probeReconciler(prom, namespace, expected, staleFactor)
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

	// Per-lane freshness. Reported in full either way: a color.Green run should record
	// WHICH lanes it saw passing, so a lane that quietly stops being declared is
	// visible in the log rather than merely absent from a failure list.
	stale := last.staleLanes()
	for _, v := range last.lanes {
		if v.FailWhy != "" {
			fmt.Printf("FAIL: lane %s — %s\n", v.Lane, v.FailWhy)
		} else {
			fmt.Printf("OK: lane %s — last success %s ago (budget %s)\n",
				v.Lane, v.Age.Round(time.Second), v.Budget)
		}
	}

	if upFail || leaderFail || len(stale) > 0 {
		dumpReconcilerDiagnostics(namespace)
		if len(stale) > 0 {
			names := make([]string, 0, len(stale))
			for _, v := range stale {
				names = append(names, v.Lane)
			}
			fmt.Fprintf(os.Stderr, "::error::reconciler lane(s) not passing: %s\n", strings.Join(names, ", "))
			if !upFail && !leaderFail {
				return fmt.Errorf("reconciler lane(s) not passing: %s", strings.Join(names, ", "))
			}
		}
		fmt.Fprintln(os.Stderr, "::error::reconciler is not functionally healthy")
		return fmt.Errorf("reconciler is not functionally healthy")
	}
	fmt.Printf("Reconciler is reporting healthy, has a leader, and all %d enabled lane(s) are fresh.\n", len(last.lanes))
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
	holder, renew, renewOK := kube.LeaseHolderRenew(obj)
	// An unreadable renewTime means not-fresh here, which is the safe direction for
	// an ASSERT (it fails rather than vouching). The elector needs the opposite
	// treatment — there, unreadable must not read as a free lease.
	if !renewOK || holder == "" || renew.IsZero() {
		return holder, false
	}
	return holder, now.Sub(renew) <= maxAge
}

// reconcilerLeaseLive reads the leader-election Lease straight from the API — the
// authoritative source, unlike the scraped gauge — and reports whether a live
// leader holds it. A kubectl error (NotFound / no access) is treated as not-live,
// so a missing Lease fails closed. Swappable for tests.
var reconcilerLeaseLive = func(namespace string, now time.Time) (holder string, live bool) {
	out, err := deps.Cluster.Run("-n", namespace, "get", "lease", reconcilerLeaseName, "-o", "json")
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
		out := strings.TrimRight(deps.Cluster.Combined(d.args...), "\n")
		if out == "" {
			out = "(no output)"
		}
		fmt.Fprintf(os.Stderr, "== %s ==\n%s\n", d.label, out)
	}
}
