package main

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

	"github.com/spf13/cobra"
)

func ciAlertEvalCmd() *cobra.Command {
	var match, prom, summary string
	var strict bool
	cmd := &cobra.Command{
		Use:   "alert-eval",
		Short: "evaluate deployed PrometheusRule alert exprs against the live Prometheus (find never-fire / false-positive rules)",
		Long: "Reads the PrometheusRule CRs off the cluster and runs each alert expr through\n" +
			"the in-cluster Prometheus /api/v1/query (via an ephemeral kubectl port-forward).\n" +
			"Classifies each as FIRING / ARMED / DEAD? / BROKEN so you can catch alerts that\n" +
			"reference a non-existent metric (promtool passes, but they never fire) or that\n" +
			"trip on a healthy cluster. Read-only.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIAlertEval(match, prom, summary, strict) },
	}
	cmd.Flags().StringVar(&match, "match", "^(LLZ|OTel|Loki|Grafana|Harbor|SupportPlane|OpenBao)",
		"RE2 regex the alert name must match (default: the landing-zone alert families)")
	cmd.Flags().StringVar(&prom, "prom", "monitoring/prometheus-operated:9090",
		"the Prometheus Service as <namespace>/<name>:<port> to port-forward to")
	cmd.Flags().StringVar(&summary, "summary", "",
		"when set, append a fenced verdict block under this title to $GITHUB_STEP_SUMMARY")
	cmd.Flags().BoolVar(&strict, "strict", false, "exit 1 if any alert is DEAD? or BROKEN")
	return cmd
}

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

	rulesJSON, err := execOutput("kubectl", "get", "prometheusrules.monitoring.coreos.com", "-A", "-o", "json")
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
	ferr := withPrometheus(prom, func(get func(string) ([]byte, error)) error {
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
		for _, r := range rules {
			raw, qerr := get("/api/v1/query?query=" + url.QueryEscape(r.Expr))
			out = append(out, classifyAlertEval(r, raw, qerr, known))
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
	}
	tally := fmt.Sprintf("alert-eval: %d alerts — FIRING=%d ARMED=%d DEAD?=%d BROKEN=%d",
		len(out), counts["FIRING"], counts["ARMED"], counts["DEAD?"], counts["BROKEN"])
	fmt.Fprintf(os.Stderr, "\n%s\n", tally)
	if counts["DEAD?"] > 0 || counts["FIRING"] > 0 {
		fmt.Fprintln(os.Stderr, "alert-eval: DEAD? = named metrics all absent (silent never-fire); FIRING on a healthy cluster = check the threshold.")
	}
	failed := strict && (counts["DEAD?"] > 0 || counts["BROKEN"] > 0)

	// When a title is given, mirror the verdict table into $GITHUB_STEP_SUMMARY so
	// the step reads as single-line glue (`llz ci alert-eval … --summary "…"`)
	// instead of a bash block that tee's stdout and re-fences it.
	if summary != "" {
		block := append([]string{fmt.Sprintf("## %s", summary), "", "```"}, lines...)
		block = append(block, tally, "```")
		if err := appendGHAFile("GITHUB_STEP_SUMMARY", block...); err != nil {
			return err
		}
	}

	if failed {
		return fmt.Errorf("alert-eval: %d DEAD? + %d BROKEN alert(s)", counts["DEAD?"], counts["BROKEN"])
	}
	return nil
}
