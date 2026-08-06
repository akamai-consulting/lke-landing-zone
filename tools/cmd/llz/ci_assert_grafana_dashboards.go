package main

// ci_assert_grafana_dashboards.go implements `llz ci assert-grafana-dashboards` —
// the gate that the landing zone's Grafana dashboards are DISCOVERABLE by the
// sidecar that imports them.
//
// A Grafana dashboard ships here as a ConfigMap carrying dashboard JSON plus the
// label the Grafana dashboard-sidecar watches. The sidecar is a label selector
// and nothing else: get the label wrong, or drop it in a refactor, and the
// ConfigMap sits in the cluster forever, perfectly valid, containing a dashboard
// nobody can see. Argo reports it Synced (the resource matches git). converge is
// color.Green. The dashboard is simply not in Grafana.
//
// It has a second, sharper edge here. The landing zone must work on BOTH stacks:
// self-installed apl-core watches `grafana_dashboard: "1"`, while managed App
// Platform's sidecar watches `release: grafana-dashboards`. A dashboard carrying
// only one label renders on one stack and silently vanishes on the other — a
// failure that no single-cluster test can see, because on the cluster you tested
// it worked.
//
// SCOPE, stated honestly. This asserts the ConfigMap exists, is labelled for BOTH
// sidecars, and carries parseable dashboard JSON. It does NOT log into Grafana to
// confirm the dashboard rendered: Grafana's admin credentials are apl-core's, and
// a gate that needed them would be unrunnable on any cluster that rotates or
// withholds them. What is asserted is everything on THIS repo's side of the
// contract — which is where the regression happens, because the sidecar's half
// is upstream and stable.
//
// FAIL-CLOSED: a missing ConfigMap, a missing label, absent data, or unparseable
// dashboard JSON all fail. Finding zero dashboards fails too.
//
// Read-only.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
	"github.com/spf13/cobra"
)

// grafanaSidecarLabels are the two dashboard-sidecar selectors a dashboard must
// satisfy to render on both stacks. Keep in step with the label block on the
// dashboard ConfigMaps (platform-apl/components/observability/*-dashboard.yaml).
var grafanaSidecarLabels = map[string]string{
	"grafana_dashboard": "1",                  // self-install: kube-prometheus-stack
	"release":           "grafana-dashboards", // managed App Platform sidecar
}

// defaultGrafanaDashboards are the landing-zone dashboard ConfigMaps that must be
// present and discoverable, as namespace/name.
//
// A KNOWN list, not "every ConfigMap carrying the label" — the label IS the thing
// under test, so a label-filtered listing would return an empty expected set the
// moment it regressed and the gate would pass color.Green on exactly that bug. Same
// reasoning as assert-scrape-targets' monitor list.
// TestDefaultGrafanaDashboardsMatchTheManifests pins this list against the
// ConfigMaps platform-apl actually ships, so a renamed or added dashboard fails
// at PR time rather than being quietly ungated.
var defaultGrafanaDashboards = []string{
	"llz-observability/llz-day2-dashboard",
	"llz-observability/llz-credential-inventory-dashboard",
}

func ciAssertGrafanaDashboardsCmd() *cobra.Command {
	var dashboards string
	var settle, interval int
	c := &cobra.Command{
		Use:   "assert-grafana-dashboards",
		Short: "fail unless the landing-zone dashboards are present and labelled for BOTH Grafana sidecars",
		Long: "Asserts each landing-zone dashboard ConfigMap exists, carries dashboard JSON\n" +
			"that parses, and is labelled for BOTH dashboard sidecars: `grafana_dashboard: \"1\"`\n" +
			"(self-installed apl-core) and `release: grafana-dashboards` (managed App\n" +
			"Platform).\n\n" +
			"The sidecar is a label selector and nothing else. Drop or mistype the label and\n" +
			"the ConfigMap sits in the cluster forever, valid and invisible — Argo reports it\n" +
			"Synced because the resource matches git, converge is color.Green, and the dashboard is\n" +
			"simply not in Grafana. Carrying only ONE of the two labels is worse: it renders\n" +
			"on the stack you tested and vanishes on the other.\n\n" +
			"Scope: everything on this repo's side of the contract. It does NOT log into\n" +
			"Grafana to confirm rendering — those credentials are apl-core's, and needing\n" +
			"them would make the gate unrunnable where they are rotated or withheld.\n\n" +
			"Read-only. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertGrafanaDashboards(cigate.SplitCSVList(dashboards),
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&dashboards, "dashboards", strings.Join(defaultGrafanaDashboards, ","),
		"comma-separated dashboard ConfigMaps (namespace/name) that must be present and discoverable")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}

// dashboardVerdict is one dashboard ConfigMap's outcome.
type dashboardVerdict struct {
	Name    string // namespace/name
	Panels  int    // panels found in the dashboard JSON (report only)
	FailWhy string
}

// evalDashboardConfigMap judges one dashboard ConfigMap. Pure.
//
// It requires BOTH sidecar labels with their exact values. Checking only for the
// key's presence would pass a `grafana_dashboard: "false"`, and checking only one
// of the two labels is the cross-stack bug this gate exists to catch.
func evalDashboardConfigMap(name string, raw []byte) dashboardVerdict {
	v := dashboardVerdict{Name: name}
	var cm struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(raw, &cm); err != nil {
		v.FailWhy = fmt.Sprintf("decoding the ConfigMap: %v", err)
		return v
	}

	var missing []string
	for _, k := range sortedMapKeys(grafanaSidecarLabels) {
		if cm.Metadata.Labels[k] != grafanaSidecarLabels[k] {
			missing = append(missing, fmt.Sprintf("%s=%q (found %q)", k, grafanaSidecarLabels[k], cm.Metadata.Labels[k]))
		}
	}
	if len(missing) > 0 {
		v.FailWhy = fmt.Sprintf("not discoverable by the dashboard sidecar — missing/incorrect %s. "+
			"The ConfigMap is present and valid, so Argo reports it Synced and converge stays color.Green; "+
			"the dashboard is simply absent from Grafana. Both labels are required: one covers the "+
			"self-installed stack, the other managed App Platform",
			strings.Join(missing, "; "))
		return v
	}

	if len(cm.Data) == 0 {
		v.FailWhy = "the ConfigMap carries no data — the sidecar would import an empty dashboard"
		return v
	}
	// Every value must parse as dashboard JSON. A sidecar handed malformed JSON
	// logs a line nobody reads and moves on, which looks identical to success.
	for _, key := range sortedMapKeys(cm.Data) {
		var dash struct {
			Panels []any  `json:"panels"`
			Title  string `json:"title"`
		}
		if err := json.Unmarshal([]byte(cm.Data[key]), &dash); err != nil {
			v.FailWhy = fmt.Sprintf("data[%q] is not valid dashboard JSON (%v) — the sidecar logs this and moves on, "+
				"so it is indistinguishable from a successful import", key, err)
			return v
		}
		v.Panels += len(dash.Panels)
	}
	return v
}

// readDashboardConfigMap fetches one dashboard ConfigMap. Seamed for tests.
var readDashboardConfigMap = func(namespace, name string) ([]byte, error) {
	return execOutput("kubectl", "-n", namespace, "get", "configmap", name, "-o", "json")
}

// probeGrafanaDashboards evaluates every expected dashboard.
func probeGrafanaDashboards(dashboards []string) []dashboardVerdict {
	out := make([]dashboardVerdict, 0, len(dashboards))
	for _, d := range dashboards {
		ns, name, ok := strings.Cut(d, "/")
		if !ok {
			out = append(out, dashboardVerdict{Name: d,
				FailWhy: "not in namespace/name form — cannot be looked up"})
			continue
		}
		raw, err := readDashboardConfigMap(ns, name)
		if err != nil {
			out = append(out, dashboardVerdict{Name: d,
				FailWhy: fmt.Sprintf("ConfigMap not found (%v) — the dashboard was never applied, or its name changed", err)})
			continue
		}
		out = append(out, evalDashboardConfigMap(d, raw))
	}
	return out
}

// failedDashboards returns the dashboards that are not discoverable.
func failedDashboards(vs []dashboardVerdict) []string {
	var out []string
	for _, v := range vs {
		if v.FailWhy != "" {
			out = append(out, v.Name)
		}
	}
	sort.Strings(out)
	return out
}

func runCIAssertGrafanaDashboards(dashboards []string, settle, interval time.Duration) error {
	fmt.Println("## Grafana dashboard-discoverability assertion")
	if len(dashboards) == 0 {
		fmt.Fprintln(os.Stderr, "::error::no --dashboards given — refusing to pass having checked nothing")
		return fmt.Errorf("no --dashboards given — refusing to pass vacuously")
	}

	var last []dashboardVerdict
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		last = probeGrafanaDashboards(dashboards)
		if len(failedDashboards(last)) == 0 {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		fmt.Printf("attempt %d: %v not discoverable yet — retrying in %s\n", attempt, failedDashboards(last), interval)
		time.Sleep(interval)
	}

	for _, v := range last {
		if v.FailWhy != "" {
			fmt.Printf("FAIL: %s — %s\n", v.Name, v.FailWhy)
		} else {
			fmt.Printf("OK: %s — labelled for both sidecars, %d panel(s)\n", v.Name, v.Panels)
		}
	}

	if bad := failedDashboards(last); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "::error::dashboard(s) not discoverable by the Grafana sidecar: %s\n", strings.Join(bad, ", "))
		return fmt.Errorf("dashboard(s) not discoverable by the Grafana sidecar: %s", strings.Join(bad, ", "))
	}
	fmt.Printf("All %d dashboard(s) are present and discoverable on both stacks.\n", len(last))
	return nil
}
