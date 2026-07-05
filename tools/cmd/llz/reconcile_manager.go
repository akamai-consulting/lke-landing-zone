// The reconciler manager for `llz reconcile` (see
// docs/designs/kube-native-reconciler.md).
//
// A reconciler is one named unit of work run on a resync interval. The manager
// runs each in its own goroutine and records a uniform per-reconciler metric set,
// so every reconciler — the observe-only sampler and the Phase 2 timed reconcilers
// (Linode cred rotation, Harbor provisioning) folded off their CronJobs — is
// visible the same way:
//
//	llz_reconcile_runs_total{reconciler}                     counter
//	llz_reconcile_errors_total{reconciler}                   counter
//	llz_reconcile_up{reconciler}                             gauge  (1 = last run ok)
//	llz_reconcile_last_success_timestamp_seconds{reconciler} gauge  (unix)
//	llz_reconcile_last_duration_seconds{reconciler}          gauge
//
// PHASE 2 folds the timed reconcilers in as BOUNDED-RESYNC loops (their state
// lives in the Linode API / Harbor / OpenBao and emits no Kubernetes watch
// events, so a timer is the right trigger — the same cadence the CronJobs ran).
// They stay OFF by default: the CronJobs remain the owners until a reconciler
// proves out per-env (the design's "keep the CronJob until one green e2e cycle"),
// so enabling one is an opt-in flag + the same env/secrets its CronJob had.
package main

import (
	"context"
	"sync"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
)

// reconciler is one named resync loop.
type reconciler struct {
	name     string
	interval time.Duration
	// run performs one reconcile pass. It must be idempotent and safe to call
	// repeatedly (the timed reconcilers are due-based: a pass with nothing due is
	// a no-op). A non-nil error marks the pass failed (up=0, errors_total++).
	run func(ctx context.Context) error
}

// runManager runs every reconciler concurrently until ctx is cancelled, then
// waits for their loops to exit. now is injected so tests drive time; production
// passes time.Now.
func runManager(ctx context.Context, reg *metrics.Registry, now func() time.Time, recs []reconciler) {
	var wg sync.WaitGroup
	for _, r := range recs {
		wg.Add(1)
		go func(r reconciler) {
			defer wg.Done()
			runReconcilerLoop(ctx, reg, now, r)
		}(r)
	}
	wg.Wait()
}

// runReconcilerLoop runs r once immediately (so metrics populate before the first
// scrape) then on every interval tick until ctx is cancelled.
func runReconcilerLoop(ctx context.Context, reg *metrics.Registry, now func() time.Time, r reconciler) {
	reconcileOnce(ctx, reg, now, r)
	if r.interval <= 0 {
		<-ctx.Done() // no cadence (e.g. a test) — just hold until shutdown
		return
	}
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcileOnce(ctx, reg, now, r)
		}
	}
}

// reconcileOnce runs one pass and records its metrics. errors_total and up
// distinguish "reporting failure" from "wedged": a failing pass still increments
// runs_total and updates up=0, so a loop that has stopped ticking is visible as a
// flat runs_total (and, for the observe reconciler, a stale success timestamp).
func reconcileOnce(ctx context.Context, reg *metrics.Registry, now func() time.Time, r reconciler) {
	lbl := map[string]string{"reconciler": r.name}
	reg.AddCounter("llz_reconcile_runs_total", "total reconcile passes per reconciler", lbl, 1)
	reg.AddCounter("llz_reconcile_errors_total", "total failed reconcile passes per reconciler", lbl, 0) // register at 0

	start := now()
	err := r.run(ctx)
	end := now()
	reg.SetGauge("llz_reconcile_last_duration_seconds", "wall-clock duration of the last reconcile pass",
		lbl, end.Sub(start).Seconds())

	if err != nil {
		reg.AddCounter("llz_reconcile_errors_total", "total failed reconcile passes per reconciler", lbl, 1)
		reg.SetGauge("llz_reconcile_up", "1 if the reconciler's last pass succeeded", lbl, 0)
		return
	}
	reg.SetGauge("llz_reconcile_up", "1 if the reconciler's last pass succeeded", lbl, 1)
	reg.SetGauge("llz_reconcile_last_success_timestamp_seconds",
		"unix time of the reconciler's most recent successful pass", lbl, float64(end.Unix()))
}
