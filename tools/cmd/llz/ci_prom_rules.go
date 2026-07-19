package main

// ci_prom_rules.go implements `llz ci health-prom-rules` — the native port of
// llz-scheduled-checks.yml's Prometheus rule-evaluation check (the last
// scheduled check that was still inline bash + python). A PrometheusRule can
// be syntactically valid yet fail at evaluation time (a missing metric, a
// label-join mistake); Prometheus only exposes that as lastError on
// /api/v1/rules, so this port-forwards the Prometheus pod and inspects every
// rule group. Warn-only, like the other health-* siblings that page via job
// summary annotations rather than blocking scheduled work.

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func ciHealthPromRulesCmd() *cobra.Command {
	var prom string
	c := &cobra.Command{
		Use:   "health-prom-rules",
		Short: "report PrometheusRule groups with evaluation errors (warn-only)",
		Long: "Queries Prometheus /api/v1/rules and reports any rule carrying a lastError to\n" +
			"the step summary + ::warning:: annotations — evaluation failures (missing\n" +
			"metric, label-join mistake) that promtool's syntax check cannot catch. Reads\n" +
			"REGION for the report headings.\n\n" +
			"Warn-only by design, but it no longer passes VACUOUSLY: an unreachable\n" +
			"Prometheus is an error, not a clean skip. It previously looked for the pod in\n" +
			"llz-observability — which holds the LLZ ServiceMonitor/PrometheusRule CRs,\n" +
			"while apl-core's Prometheus runs in monitoring — so it took its skip path on\n" +
			"every run and nothing ever validated the live rules.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIHealthPromRules(prom) },
	}
	c.Flags().StringVar(&prom, "prom", "monitoring/prometheus-operated:9090",
		"Prometheus to query, as <namespace>/<service-or-pod>:<port>")
	return c
}

// promRulesJSON is the slice of the /api/v1/rules response the check reads.
type promRulesJSON struct {
	// Status/Error are Prometheus's own envelope. Without them an
	// {"status":"error"} body unmarshalled CLEANLY with zero groups, and the
	// caller reported "All Prometheus rule groups evaluated without errors" —
	// an affirmative claim derived from a failure.
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		Groups []struct {
			Name  string `json:"name"`
			Rules []struct {
				Name      string `json:"name"`
				LastError string `json:"lastError"`
			} `json:"rules"`
		} `json:"groups"`
	} `json:"data"`
}

// ruleEvalErrors extracts "group/rule: lastError" lines. Pure, so the
// extraction is unit-testable on canned API responses.
func ruleEvalErrors(body []byte) []string {
	var rules promRulesJSON
	if json.Unmarshal(body, &rules) != nil {
		return nil
	}
	var errs []string
	for _, g := range rules.Data.Groups {
		for _, r := range g.Rules {
			if r.LastError != "" {
				name := r.Name
				if name == "" {
					name = "?"
				}
				errs = append(errs, fmt.Sprintf("%s/%s: %s", g.Name, name, r.LastError))
			}
		}
	}
	return errs
}

func runCIHealthPromRules(prom string) error {
	region := os.Getenv("REGION")

	// Route through the shared withPrometheus seam, like alert-eval /
	// assert-scrape-targets / assert-reconciler / prom-metrics. That fixes the
	// namespace (this used to look in llz-observability, which holds the LLZ CRs —
	// apl-core's Prometheus lives in monitoring) and drops a hand-rolled transport
	// that pinned local port 19090, never drained the port-forward's stdout, and
	// had its own readiness poll.
	var body []byte
	if err := withPrometheus(prom, func(get func(string) ([]byte, error)) error {
		raw, err := get("/api/v1/rules")
		if err != nil {
			return err
		}
		body = raw
		return nil
	}); err != nil {
		// NOT a clean skip. This check's whole job is to notice rules that fail to
		// evaluate; if it cannot ask, it has established nothing, and returning nil
		// would report green. The scheduled job is continue-on-error, so a genuinely
		// unreachable cluster still won't block other work — it will just be visible.
		return fmt.Errorf("health-prom-rules: could not query %s on %s: %w", prom, region, err)
	}
	// Zero groups is not "no errors" — it is the ruleSelector regression
	// monitoring-label-guard exists to catch (a PrometheusRule whose
	// `prometheus: system` label is missing is never LOADED, so it evaluates
	// nothing and reports nothing). Refuse to call that healthy.
	var doc promRulesJSON
	if uErr := json.Unmarshal(body, &doc); uErr != nil {
		return fmt.Errorf("health-prom-rules: could not parse the /api/v1/rules response on %s: %w", region, uErr)
	}
	if doc.Status != "" && doc.Status != "success" {
		detail := doc.Error
		if detail == "" {
			detail = "status=" + doc.Status
		}
		return fmt.Errorf("health-prom-rules: Prometheus returned an error for /api/v1/rules on %s: %s", region, detail)
	}
	if len(doc.Data.Groups) == 0 {
		return fmt.Errorf("health-prom-rules: Prometheus has ZERO rule groups loaded on %s — "+
			"not 'no evaluation errors' but nothing to evaluate. A PrometheusRule missing its `prometheus: system` "+
			"label is never selected, so it loads no groups and reports no errors", region)
	}
	errored := ruleEvalErrors(body)

	summary := []string{"", fmt.Sprintf("### Prometheus Rule Evaluation — %s", region), ""}
	if len(errored) == 0 {
		fmt.Printf("All Prometheus rule groups evaluated without errors on %s.\n", region)
		return appendGHAFile("GITHUB_STEP_SUMMARY",
			append(summary, "- All rule groups: no evaluation errors")...)
	}
	for _, line := range errored {
		fmt.Fprintf(os.Stderr, "::warning::Rule evaluation error (%s): %s\n", region, line)
	}
	summary = append(summary, "**Rules with evaluation errors:**", "```")
	summary = append(summary, errored...)
	summary = append(summary, "```")
	return appendGHAFile("GITHUB_STEP_SUMMARY", summary...)
}
