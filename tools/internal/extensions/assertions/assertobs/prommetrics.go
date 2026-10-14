package assertobs

// ci_prom_metrics.go implements `llz ci prom-metrics` — a cluster diagnostic that
// lists the metric NAMES the in-cluster Prometheus is scraping, filtered by a
// regex. Its job is metric-name DISCOVERY: writing an error-rate/saturation alert
// blind risks a silent non-firing rule (promtool checks syntax, not existence),
// so this dumps the real exporter metric names (loki_*, otelcol_*, harbor_*,
// vault_*, …) off a live cluster so the alert exprs can be written against names
// that actually exist. Best-effort + read-only: reaches Prometheus via an
// ephemeral kubectl port-forward (see prom_query.go — the apiserver Service proxy
// is webhook-denied on LKE-Enterprise), so it needs only the health-check kubeconfig.

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
)

func runCIPromMetrics(match, prom string) error {
	re, err := regexp.Compile(match)
	if err != nil {
		return fmt.Errorf("invalid --match regex: %w", err)
	}
	var names []string
	err = WithPrometheus(prom, func(get func(string) ([]byte, error)) error {
		out, gerr := get("/api/v1/label/__name__/values")
		if gerr != nil {
			return gerr
		}
		names = filterPromMetricNames(out, re)
		return nil
	})
	if err != nil {
		// Non-fatal: a wrong Service / Prometheus not up yet shouldn't fail a
		// keep_cluster diagnostic. Report where it looked so the operator can retry
		// with a different --prom against the (kept) cluster.
		fmt.Fprintf(os.Stderr, "prom-metrics: could not reach Prometheus at %s (%v) — retry with --prom <ns>/<svc>:<port>\n", prom, err)
		return nil
	}
	for _, n := range names {
		fmt.Println(n)
	}
	fmt.Fprintf(os.Stderr, "prom-metrics: %d metric name(s) match %q\n", len(names), match)
	return nil
}

// filterPromMetricNames parses the /label/__name__/values response and returns
// the sorted, de-duplicated names matching re.
func filterPromMetricNames(raw []byte, re *regexp.Regexp) []string {
	var resp struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range resp.Data {
		if re.MatchString(n) && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
