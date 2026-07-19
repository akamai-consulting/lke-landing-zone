package main

// ci_monitoring_label_guard.go implements `llz ci monitoring-label-guard` — the
// static guard extracted from the day-2-observability-blind outage (#175).
//
// apl-core v6's Prometheus selects monitoring CRs by {prometheus: system} — its
// serviceMonitorSelector, ruleSelector, AND podMonitorSelector all match that one
// label. LLZ asks apl-core for empty (discover-all) selectors via _rawValues, but
// apl-core overrides that (confirmed live), so a ServiceMonitor / PodMonitor /
// PrometheusRule WITHOUT the label is silently ignored: its metrics are never
// scraped, its alert rules are never loaded, and its alerts never fire — while
// promtool (rule syntax) and kube-linter (manifest shape) both pass. #175 was
// exactly this: 5 CRs (2 ServiceMonitors + 3 PrometheusRules) missing the label
// left the entire in-cluster day-2 signal blind, undetectable except on a live
// cluster.
//
// The guard makes that class a PR-time failure. It scans FINAL YAML only: the
// instance-template component manifests plus the rendered/ chart output (run
// `make render-charts` first — the openbao ServiceMonitor is a chart template
// whose label renders from serviceMonitor.selectorLabels). Go TEMPLATE dirs
// (kubernetes-charts/*/templates) are skipped: they contain `{{ }}`, not YAML.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// monitoringGuardKinds are the CR kinds apl-core's Prometheus label-selects.
var monitoringGuardKinds = map[string]bool{
	"ServiceMonitor": true,
	"PodMonitor":     true,
	"PrometheusRule": true,
}

const (
	requiredMonitoringLabelKey = "prometheus"
	requiredMonitoringLabelVal = "system"
)

type monitoringDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name   string            `yaml:"name"`
		Labels map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
}

type monitoringLabelFinding struct {
	file, kind, name string
}

func ciMonitoringLabelGuardCmd() *cobra.Command {
	var roots []string
	cmd := &cobra.Command{
		Use:   "monitoring-label-guard",
		Short: "every ServiceMonitor/PodMonitor/PrometheusRule must carry `prometheus: system` (apl-core's selector)",
		Long: "Fails if any ServiceMonitor, PodMonitor, or PrometheusRule in the scanned\n" +
			"trees lacks the label `prometheus: system`. apl-core's Prometheus selects\n" +
			"monitoring CRs by that label; one without it is silently ignored (metrics\n" +
			"unscraped / rules unloaded / alerts never firing) — a class promtool and\n" +
			"kube-linter cannot see. Scans final YAML; run `make render-charts` first so\n" +
			"the rendered chart output (e.g. the openbao ServiceMonitor) is included.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runMonitoringLabelGuard(roots) },
	}
	cmd.Flags().StringSliceVar(&roots, "root", []string{"platform-apl", "rendered"},
		"directories to scan (final YAML only; run render-charts to populate rendered/)")
	return cmd
}

func runMonitoringLabelGuard(roots []string) error {
	findings, examined, err := collectMonitoringLabelFindings(roots)
	if err != nil {
		return err
	}
	// This guard is the one that most needed the empty-corpus check: the openbao
	// ServiceMonitor renders its `prometheus: system` label from
	// serviceMonitor.selectorLabels, so only the RENDERED tree carries the real
	// value — and rendered/ not being built was exactly the silently-skipped case.
	if err := requireCorpus("monitoring-label-guard", examined, roots); err != nil {
		return err
	}
	if len(findings) == 0 {
		fmt.Println("monitoring-label-guard: all ServiceMonitors/PodMonitors/PrometheusRules carry `prometheus: system`.")
		return nil
	}
	for _, f := range findings {
		// ::error file=…:: so the finding lands as a PR annotation. This printed a
		// plain indented line, so the guard failed the build with its reasons buried
		// in log output — an odd shape for a check that exists because #175 was a
		// silently-ignored signal.
		fmt.Printf("::error file=%s::%s %q lacks `prometheus: system` — apl-core's Prometheus selects on that label, so this CR is silently ignored (metrics unscraped / rules unloaded)\n",
			f.file, f.kind, f.name)
	}
	return fmt.Errorf("monitoring-label-guard: %d monitoring CR(s) lack `prometheus: system` — "+
		"apl-core's Prometheus will silently ignore them (metrics unscraped / rules unloaded)", len(findings))
}

func collectMonitoringLabelFindings(roots []string) (findings []monitoringLabelFinding, examined int, err error) {
	examined, err = walkManifests(roots, func(path string, raw []byte) error {
		for _, doc := range splitMonitoringDocs(string(raw)) {
			if monitoringGuardKinds[doc.Kind] &&
				doc.Metadata.Labels[requiredMonitoringLabelKey] != requiredMonitoringLabelVal {
				findings = append(findings, monitoringLabelFinding{file: path, kind: doc.Kind, name: doc.Metadata.Name})
			}
		}
		return nil
	})
	if err != nil {
		return nil, examined, err
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].file != findings[j].file {
			return findings[i].file < findings[j].file
		}
		return findings[i].name < findings[j].name
	})
	return findings, examined, nil
}

// splitMonitoringDocs parses a multi-doc YAML file, skipping docs that fail to
// parse (kustomize patches etc. are not this guard's concern).
func splitMonitoringDocs(raw string) []monitoringDoc {
	var docs []monitoringDoc
	dec := yaml.NewDecoder(strings.NewReader(raw))
	for {
		var d monitoringDoc
		if err := dec.Decode(&d); err != nil {
			break
		}
		if d.Kind != "" {
			docs = append(docs, d)
		}
	}
	return docs
}
