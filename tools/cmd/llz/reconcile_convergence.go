// The convergence gauge — the observe reconciler's day-2 read of "is the cluster
// converged?", surfaced as a metric (see docs/designs/kube-native-reconciler.md).
//
// It reuses the SAME tested predicate `llz ci health` uses — internal/health's
// ParseArgoApp + ClassifyArgoApp — over Argo CD Application status, which is the
// canonical convergence signal (the convergence contract's readiness gate waits on
// the bootstrap Application being Synced+Healthy). The exit-code CLI stays the
// source of truth for the Terraform gate; this publishes the same 0/1/2
// classification as a gauge so day-2 convergence is continuously observable,
// Alertmanager-routable, and not only visible when a workflow runs. It OBSERVES —
// it drives nothing (convergence-contract-clean; anti-pattern #4 is the opposite).
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
)

// sampleConvergence lists Argo CD Applications, classifies each via the shared
// internal/health predicate, and publishes the convergence gauges. It returns an
// error only on an API-read failure (so the observe reconciler records up=0); a
// hard-failed *cluster* is a successful observation (state=1), not a sampler
// failure. A 404 on the collection means the Applications CRD is not installed
// yet (pre-bootstrap) — reported as in-progress, not an error.
func sampleConvergence(ctx context.Context, client nodeGetter, reg *metrics.Registry) error {
	obj, status, err := client.GetJSON(ctx, argoAppsPath)
	if err != nil {
		return err
	}
	if status == 404 {
		reg.SetGauge("llz_convergence_state", convergenceStateHelp, nil, float64(health.InProgress.ExitCode()))
		return nil
	}
	if status < 200 || status >= 300 || obj == nil {
		return fmt.Errorf("GET applications: status %d", status)
	}

	items, _ := obj["items"].([]any)
	var r health.Report
	for _, it := range items {
		raw, err := json.Marshal(it)
		if err != nil {
			continue
		}
		app, err := health.ParseArgoApp(raw)
		if err != nil {
			continue
		}
		cat, msg := health.ClassifyArgoApp(app, false) // day-2: not phase-1 bootstrap
		r.Add(cat, msg)
	}

	reg.SetGauge("llz_convergence_state", convergenceStateHelp, nil, float64(r.ExitCode()))
	reg.SetGauge("llz_convergence_apps_failed",
		"count of Argo Applications classified hard-failed", nil, float64(len(r.Failed)))
	reg.SetGauge("llz_convergence_apps_pending",
		"count of Argo Applications still reconciling (in-progress)", nil, float64(len(r.Pending)))
	return nil
}

const convergenceStateHelp = "cluster convergence per llz ci health: 0 converged, 1 hard-failed, 2 in-progress"
