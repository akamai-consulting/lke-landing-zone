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
	s, err := convergenceSample(ctx, client)
	if err != nil {
		return err
	}
	r, crdPresent := s.report, s.crdPresent
	if !crdPresent {
		reg.SetGauge("llz_convergence_state", convergenceStateHelp, nil, float64(health.InProgress.ExitCode()))
		// Publish the counts on this path too. They are upsert-only (the registry
		// never expires a sample), so returning early left the PREVIOUS run's
		// failed/pending/observed counts on the endpoint indefinitely — a dashboard
		// reading "3 apps observed" for a cluster whose Application CRD is gone.
		setConvergenceCounts(reg, 0, 0, 0)
		return nil
	}
	reg.SetGauge("llz_convergence_state", convergenceStateHelp, nil, float64(r.ExitCode()))
	setConvergenceCounts(reg, len(r.Failed), len(r.Pending), s.observed)
	return nil
}

// setConvergenceCounts publishes the corpus gauges. llz_convergence_apps_observed
// is what lets an alert tell "state 0 because everything is healthy" from "state 0
// because nothing was read" — see LLZConvergenceNoApps in the reconciler
// PrometheusRule. Without it llz_convergence_state==0 is unfalsifiable.
func setConvergenceCounts(reg *metrics.Registry, failed, pending, observed int) {
	reg.SetGauge("llz_convergence_apps_failed",
		"count of Argo Applications classified hard-failed", nil, float64(failed))
	reg.SetGauge("llz_convergence_apps_pending",
		"count of Argo Applications still reconciling (in-progress)", nil, float64(pending))
	reg.SetGauge("llz_convergence_apps_observed",
		"count of Argo Applications successfully parsed and classified this sample", nil, float64(observed))
}

// convergenceReport classifies Argo CD Application health into the convergence
// verdict over internal/kube (no kubectl) — the SHARED core of the observe
// reconciler's gauge (sampleConvergence) and the `llz ci health-incluster`
// exit-code verb. Argo Application status is the canonical convergence signal (the
// convergence contract's readiness gate waits on it), classified through the same
// unit-tested health.ClassifyArgoApp predicate `llz ci health` uses. crdPresent is
// false when the Application CRD is not yet registered — pre-bootstrap, which is
// in-progress (not converged).
func convergenceReport(ctx context.Context, client nodeGetter) (health.Report, bool, error) {
	s, err := convergenceSample(ctx, client)
	return s.report, s.crdPresent, err
}

// convergenceSampleResult is one read of the Applications collection: the
// classification, plus how much of the collection actually reached it. The
// corpus counts are the point — see convergenceSample.
type convergenceSampleResult struct {
	report     health.Report
	crdPresent bool
	observed   int // Applications parsed and classified
	unparsed   int // Applications the API returned that we could not read
}

// convergenceSample is convergenceReport with the corpus accounted for.
//
// health.Report.Verdict() treats an EMPTY report as Converged, which is correct
// for `llz ci health` — there the report accumulates many independent checks, and
// "nothing to report" means every one of them passed. Here the report has exactly
// one source, the Argo Application loop, so empty does not mean "all evidence
// positive", it means "no evidence". Zero parsed apps used to render as
// llz_convergence_state=0 and `health-incluster` exit 0 — "all Argo Applications
// converged" — for a 200 with an empty items[], a missing items key, or a list
// whose every entry failed to parse (both loop failures were silent `continue`s).
//
// So the corpus is counted and an unread one is recorded as Pending, which the
// existing contract already maps to in-progress: the gate keeps polling instead of
// passing, and no downstream caller needs a special case. This is requireCorpus
// (guard_corpus.go) and sectionItems (ci_health.go) for the reconciler lane —
// a check that examined nothing must not report the same green as one that
// examined everything.
func convergenceSample(ctx context.Context, client nodeGetter) (convergenceSampleResult, error) {
	obj, status, err := client.GetJSON(ctx, argoAppsPath)
	if err != nil {
		return convergenceSampleResult{}, err
	}
	if status == 404 {
		return convergenceSampleResult{}, nil
	}
	if status < 200 || status >= 300 || obj == nil {
		return convergenceSampleResult{}, fmt.Errorf("GET applications: status %d", status)
	}
	s := convergenceSampleResult{crdPresent: true}
	items, ok := obj["items"].([]any)
	if !ok && obj["items"] != nil {
		// A present-but-not-an-array items key is a malformed response, not an
		// empty cluster. The discarded type assertion read it as "no apps".
		s.report.AddPending("Applications list returned a non-array .items — cannot classify convergence")
		return s, nil
	}
	for _, it := range items {
		raw, err := json.Marshal(it)
		if err != nil {
			s.unparsed++
			continue
		}
		app, err := health.ParseArgoApp(raw)
		if err != nil {
			s.unparsed++
			continue
		}
		s.observed++
		cat, msg := health.ClassifyArgoApp(app, false) // day-2: not phase-1 bootstrap
		s.report.Add(cat, msg)
	}
	if s.unparsed > 0 {
		s.report.AddPending(fmt.Sprintf(
			"%d Argo Application(s) could not be parsed — their health is unknown, not healthy", s.unparsed))
	}
	if s.observed == 0 && s.unparsed == 0 {
		// The Application CRD is registered but the collection is empty: bootstrap
		// has not created the Applications yet. Same state as the 404 — in-progress,
		// which is what the CRD-absent path already reports.
		s.report.AddPending("no Argo Applications exist yet — bootstrap has not created them (nothing to converge)")
	}
	return s, nil
}

const convergenceStateHelp = "cluster convergence per llz ci health: 0 converged, 1 hard-failed, 2 in-progress"
