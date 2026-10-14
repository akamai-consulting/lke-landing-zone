package assertobs

// ci_alert_eval.go implements `llz ci alert-eval` — a live-cluster diagnostic that
// EVALUATES every deployed PrometheusRule alert expr against the in-cluster
// Prometheus, instead of only syntax-checking it. promtool validates that a rule
// parses; it cannot tell you the rule references a metric/label that does not
// exist (a silent never-fires bug) or that the threshold trips on a healthy
// cluster (a false positive). This reads the PrometheusRule CRs off the cluster,
// runs each expr through /api/v1/query, and classifies the outcome:
//
//   FIRING   the expr returns series NOW — on a healthy cluster, a likely
//            false-positive threshold worth investigating.
//   ARMED    empty result, but at least one metric the expr names exists — the
//            healthy state (rule is wired and simply not tripping).
//   DEAD?    empty result AND none of the metrics the expr names exist in the
//            live metric set — the silent-never-fires signature. Investigate.
//   BROKEN   Prometheus rejected the expr (bad PromQL / label that errors).
//
// ARMED rules additionally get a SELECTOR probe (alertselectors.go), because ARMED
// is where the expensive bug hides: `LokiStatefulSetUnavailable` selected
// `statefulset="loki"` on a cluster whose StatefulSet is `loki-ingester`, so the
// metric NAME existed, DEAD? did not trigger, and the rule graded ARMED — the
// healthy verdict — through a 16-day log-ingestion outage. An ARMED rule whose
// every label selector matches zero series is annotated NOMATCH: reported, never
// gating, for the reason given in alertselectors.go.
//
// Reaches Prometheus via an ephemeral kubectl port-forward (see prom_query.go —
// the apiserver Service proxy is webhook-denied on LKE-Enterprise), same as
// `llz ci prom-metrics`. The `for:` duration is not part of the expr, so this
// reports whether the CONDITION is currently true (would-fire ignoring `for`).

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
)

type evalRule struct {
	Namespace string
	Group     string
	Alert     string
	Expr      string
}

type evalVerdict struct {
	rule    evalRule
	verdict string // FIRING | ARMED | DEAD? | BROKEN
	value   string // first sample value when FIRING, else ""
	detail  string // error text for BROKEN
	// deadSelectors names every label selector in the expr when ALL of them match
	// zero series — the signature of a rule pointed at a renamed workload, which
	// the name-level DEAD? check grades ARMED. Reported, never gating; see
	// alertselectors.go for why.
	deadSelectors []string
}

// vacuous reports a check that could not actually be performed. Report-only mode
// warns and passes (the report is the deliverable; a diagnostic that can't reach
// the cluster is not a finding). --strict mode FAILS.
//
// Without this split, --strict was unfalsifiable four different ways: any of the
// four inputs below could be unavailable and the verb would still exit 0, having
// evaluated nothing. A gate that passes when it cannot run is worse than no gate,
// because it reads as evidence.
func vacuous(strict bool, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if strict {
		return fmt.Errorf("alert-eval --strict: %s — refusing to pass without evaluating the rules", msg)
	}
	fmt.Fprintf(os.Stderr, "alert-eval: %s\n", msg)
	return nil
}

func runCIAlertEval(match, prom, summary string, strict bool) error {
	re, err := regexp.Compile(match)
	if err != nil {
		return fmt.Errorf("invalid --match regex: %w", err)
	}

	rulesJSON, err := caps.Exec("kubectl", "get", "prometheusrules.monitoring.coreos.com", "-A", "-o", "json")
	if err != nil {
		return vacuous(strict, "could not list PrometheusRules (%v) — is this pointed at the cluster?", err)
	}
	rules := parseAlertRules(rulesJSON, re)
	if len(rules) == 0 {
		return vacuous(strict, "no alert rules match %q, so nothing would be evaluated", match)
	}

	// One port-forward session serves the metric-name fetch AND every per-expr
	// query (20+), instead of a fresh kubectl per query.
	var out []evalVerdict
	ferr := WithPrometheus(prom, func(get func(string) ([]byte, error)) error {
		// The full metric-name set powers DEAD? detection (an expr whose named
		// metrics are all absent can never fire). If this fetch fails, `known` is
		// empty and exprMetricsExist stops claiming DEAD? at all — which silently
		// zeroes one of the two verdicts --strict gates on. Report-only tolerates
		// that (and says so); --strict must not.
		known := map[string]bool{}
		nameJSON, nerr := get("/api/v1/label/__name__/values")
		if nerr != nil {
			if strict {
				return fmt.Errorf("metric-name fetch failed (%v): DEAD? detection would be disabled", nerr)
			}
			fmt.Fprintf(os.Stderr, "alert-eval: metric-name fetch failed (%v) — DEAD? detection disabled for this run\n", nerr)
		}
		for _, n := range parsePromLabelValues(nameJSON) {
			known[n] = true
		}
		// A tiny cache: sibling alerts in one group routinely share a selector
		// (every Loki rule scopes to namespace="monitoring"), and each probe is a
		// port-forwarded round trip.
		seen := map[string][2]bool{}
		matcher := func(sel string) (matches, answered bool) {
			if v, ok := seen[sel]; ok {
				return v[0], v[1]
			}
			raw, err := get("/api/v1/query?query=" + url.QueryEscape("count("+sel+")"))
			m, a := selectorHasSeries(raw, err)
			seen[sel] = [2]bool{m, a}
			return m, a
		}
		for _, r := range rules {
			raw, qerr := get("/api/v1/query?query=" + url.QueryEscape(r.Expr))
			v := classifyAlertEval(r, raw, qerr, known)
			// Only ARMED rules are worth asking about. FIRING matched something by
			// definition, DEAD? is already the louder finding, and BROKEN means the
			// expr does not run at all.
			if v.verdict == "ARMED" {
				v.deadSelectors = unmatchedSelectors(r.Expr, matcher)
			}
			out = append(out, v)
		}
		return nil
	})
	if ferr != nil {
		return vacuous(strict, "could not evaluate against Prometheus at %s (%v)", prom, ferr)
	}
	return printAlertEval(out, summary, strict)
}

// parseAlertRules extracts alert rules (not recording rules) from a
// `kubectl get prometheusrules -o json` payload, keeping those whose alert name
// matches re.
func parseAlertRules(raw []byte, re *regexp.Regexp) []evalRule {
	var doc struct {
		Items []struct {
			Metadata struct{ Namespace string } `json:"metadata"`
			Spec     struct {
				Groups []struct {
					Name  string `json:"name"`
					Rules []struct {
						Alert string `json:"alert"`
						Expr  string `json:"expr"`
					} `json:"rules"`
				} `json:"groups"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	var out []evalRule
	for _, it := range doc.Items {
		for _, g := range it.Spec.Groups {
			for _, rl := range g.Rules {
				if rl.Alert == "" || rl.Expr == "" || !re.MatchString(rl.Alert) {
					continue // recording rules have no .alert; skip non-matching
				}
				out = append(out, evalRule{it.Metadata.Namespace, g.Name, rl.Alert, rl.Expr})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Alert < out[j].Alert
	})
	return out
}

func parsePromLabelValues(raw []byte) []string {
	var resp struct {
		Data []string `json:"data"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return nil
	}
	return resp.Data
}

var promIdentRe = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)

// exprMetricsExist reports whether at least one identifier in the expr is a known
// metric name. Label keys, function names, and keywords are harmless: they simply
// won't be in the known-metric set, so the intersection is the filter.
func exprMetricsExist(expr string, known map[string]bool) bool {
	if len(known) == 0 {
		// Unknown metric set → don't claim DEAD?. This is REPORT-ONLY behavior:
		// runCIAlertEval fails under --strict rather than reaching here with an
		// empty set, because doing so would zero the DEAD? count and quietly
		// disable half of what --strict gates on.
		return true
	}
	for _, id := range promIdentRe.FindAllString(expr, -1) {
		if known[id] {
			return true
		}
	}
	return false
}

// classifyAlertEval turns a single expr's /query response into a verdict.
func classifyAlertEval(r evalRule, raw []byte, qerr error, known map[string]bool) evalVerdict {
	if qerr != nil {
		return evalVerdict{rule: r, verdict: "BROKEN", detail: qerr.Error()}
	}
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return evalVerdict{rule: r, verdict: "BROKEN", detail: "unparseable query response"}
	}
	if resp.Status != "success" {
		return evalVerdict{rule: r, verdict: "BROKEN", detail: resp.Error}
	}
	if len(resp.Data.Result) > 0 {
		val := ""
		if v := resp.Data.Result[0].Value; len(v) == 2 {
			val, _ = v[1].(string)
		}
		return evalVerdict{rule: r, verdict: "FIRING", value: val}
	}
	if exprMetricsExist(r.Expr, known) {
		return evalVerdict{rule: r, verdict: "ARMED"}
	}
	return evalVerdict{rule: r, verdict: "DEAD?"}
}

func printAlertEval(out []evalVerdict, summary string, strict bool) error {
	counts := map[string]int{}
	nomatch := 0
	lines := make([]string, 0, len(out))
	for _, v := range out {
		counts[v.verdict]++
		line := fmt.Sprintf("%-7s %s/%s", v.verdict, v.rule.Namespace, v.rule.Alert)
		switch v.verdict {
		case "FIRING":
			line += fmt.Sprintf("  value=%s", v.value)
		case "BROKEN":
			line += fmt.Sprintf("  (%s)", v.detail)
		}
		lines = append(lines, line)
		fmt.Println(line)
		if len(v.deadSelectors) > 0 {
			nomatch++
			note := "  " + selectorFinding(v.rule.Namespace+"/"+v.rule.Alert, v.deadSelectors)
			lines = append(lines, note)
			fmt.Println(note)
		}
	}
	tally := fmt.Sprintf("alert-eval: %d alerts — FIRING=%d ARMED=%d DEAD?=%d BROKEN=%d NOMATCH=%d",
		len(out), counts["FIRING"], counts["ARMED"], counts["DEAD?"], counts["BROKEN"], nomatch)
	fmt.Fprintf(os.Stderr, "\n%s\n", tally)
	if counts["DEAD?"] > 0 || counts["FIRING"] > 0 || nomatch > 0 {
		fmt.Fprintln(os.Stderr, "alert-eval: DEAD? = named metrics all absent (silent never-fire); FIRING on a healthy cluster = check the threshold.")
	}
	if nomatch > 0 {
		// NOT part of the exit status, and said out loud so nobody has to read the
		// source to find out. A selector matching nothing is often healthy (a
		// counter that never incremented), so gating would make this job red on
		// good clusters and it would stop being read — which is how the outage it
		// is named for went unnoticed in the first place.
		fmt.Fprintf(os.Stderr, "alert-eval: %d alert(s) read ARMED but select ZERO series (NOMATCH). "+
			"Reported, not gating. Each is either a rule pointed at a renamed workload or a metric "+
			"that has legitimately never been emitted — the two look identical from here, so a human "+
			"has to look.\n", nomatch)
	}
	failed := strict && (counts["DEAD?"] > 0 || counts["BROKEN"] > 0)

	// When a title is given, mirror the verdict table into $GITHUB_STEP_SUMMARY so
	// the step reads as single-line glue (`llz ci alert-eval … --summary "…"`)
	// instead of a bash block that tee's stdout and re-fences it.
	if summary != "" {
		block := append([]string{fmt.Sprintf("## %s", summary), "", "```"}, lines...)
		block = append(block, tally, "```")
		if err := caps.Summary("GITHUB_STEP_SUMMARY", block...); err != nil {
			return err
		}
	}

	if failed {
		return fmt.Errorf("alert-eval: %d DEAD? + %d BROKEN alert(s)", counts["DEAD?"], counts["BROKEN"])
	}
	return nil
}
