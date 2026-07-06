package main

// ci_prom_metrics.go implements `llz ci prom-metrics` — a cluster diagnostic that
// lists the metric NAMES the in-cluster Prometheus is scraping, filtered by a
// regex. Its job is metric-name DISCOVERY: writing an error-rate/saturation alert
// blind risks a silent non-firing rule (promtool checks syntax, not existence),
// so this dumps the real exporter metric names (loki_*, otelcol_*, harbor_*,
// vault_*, …) off a live cluster so the alert exprs can be written against names
// that actually exist. Best-effort + read-only: reaches Prometheus through the
// apiserver Service proxy (`kubectl get --raw`), so it needs only the kubeconfig
// the health checks already use — no port-forward.

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

func ciPromMetricsCmd() *cobra.Command {
	var match, prom string
	cmd := &cobra.Command{
		Use:   "prom-metrics",
		Short: "list in-cluster Prometheus metric names matching a regex (metric-name discovery)",
		Long: "Queries the in-cluster Prometheus (via the apiserver Service proxy, no\n" +
			"port-forward) for every scraped metric name and prints those matching --match.\n" +
			"Use it to discover the real exporter metric names (loki_*, otelcol_*, harbor_*)\n" +
			"before writing an error-rate/saturation alert — promtool validates syntax, not\n" +
			"that a metric exists. Read-only; best-effort (exit 0 even on no matches).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIPromMetrics(match, prom) },
	}
	cmd.Flags().StringVar(&match, "match", ".", "RE2 regex the metric name must match")
	cmd.Flags().StringVar(&prom, "prom", "monitoring/prometheus-operated:9090",
		"the Prometheus Service as <namespace>/<name>:<port> to proxy through the apiserver")
	return cmd
}

func runCIPromMetrics(match, prom string) error {
	re, err := regexp.Compile(match)
	if err != nil {
		return fmt.Errorf("invalid --match regex: %w", err)
	}
	ns, svcPort, ok := strings.Cut(prom, "/")
	if !ok {
		return fmt.Errorf("--prom must be <namespace>/<name>:<port>, got %q", prom)
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/services/%s/proxy/api/v1/label/__name__/values", ns, svcPort)
	out, err := execOutput("kubectl", "get", "--raw", path)
	if err != nil {
		// Non-fatal: a wrong Service path / Prometheus not up yet shouldn't fail a
		// keep_cluster diagnostic. Report where it looked so the operator can retry
		// with a different --prom against the (kept) cluster.
		fmt.Fprintf(os.Stderr, "prom-metrics: could not reach Prometheus at %s (%v) — retry with --prom <ns>/<svc>:<port>\n", prom, err)
		return nil
	}
	names := filterPromMetricNames(out, re)
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
