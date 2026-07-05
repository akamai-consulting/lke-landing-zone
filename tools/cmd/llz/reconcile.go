// `llz reconcile` is the in-cluster reconciler process (see
// docs/designs/kube-native-reconciler.md). It is the long-lived counterpart to
// the `ci <verb>` one-shots the polling CronJobs run today: instead of a fixed-
// interval CronJob reaching in, one leader-elected Deployment watches cluster
// state and exposes a Prometheus metrics surface so the cluster self-reports.
//
// PHASE 0 (this file) is OBSERVE-ONLY — it drives nothing. It samples a small
// set of cluster signals on an interval and publishes them at :8080/metrics, so
// the metrics + Alertmanager path can be validated (and the CIDR-fragile daily
// hosted-runner health port-forwards demoted to belt-and-suspenders) BEFORE any
// reconciler that mutates state is migrated off its CronJob in Phase 1. It uses
// internal/kube (the hand-rolled REST client — no client-go) and internal/metrics
// (hand-rolled text exposition — no prometheus/client_golang), staying inside the
// module's deliberately-lean dependency stance.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kube"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
	"github.com/spf13/cobra"
)

func reconcileCmd() *cobra.Command {
	var metricsAddr string
	var interval int
	c := &cobra.Command{
		Use:   "reconcile",
		Short: "run the in-cluster reconciler + Prometheus metrics surface (Phase 0: observe-only)",
		Long: "Long-lived in-cluster process that samples cluster convergence signals and\n" +
			"serves them at --metrics-addr/metrics for Prometheus to scrape. Phase 0 is\n" +
			"observe-only: it publishes metrics and drives nothing. Runs from the slim\n" +
			"distroless image with an in-pod ServiceAccount (KUBERNETES_SERVICE_HOST +\n" +
			"the mounted SA token/CA). Terminates gracefully on SIGTERM.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := kube.NewInCluster()
			if err != nil {
				return err
			}
			return runReconcile(cmd.Context(), client, reconcileOpts{
				metricsAddr:    metricsAddr,
				sampleInterval: time.Duration(interval) * time.Second,
			})
		},
	}
	c.Flags().StringVar(&metricsAddr, "metrics-addr", ":8080", "address to serve the Prometheus /metrics endpoint on")
	c.Flags().IntVar(&interval, "sample-interval", 30, "seconds between cluster samples")
	return c
}

type reconcileOpts struct {
	metricsAddr    string
	sampleInterval time.Duration
}

// nodeGetter is the slice of *kube.Client the sampler needs — narrowed to an
// interface so tests can drive it with a fake or an httptest-backed real client.
type nodeGetter interface {
	GetJSON(ctx context.Context, path string) (map[string]any, int, error)
}

// runReconcile serves the metrics endpoint and samples on an interval until the
// context is cancelled (SIGTERM in a pod), then shuts the server down cleanly.
func runReconcile(ctx context.Context, client nodeGetter, o reconcileOpts) error {
	if o.sampleInterval <= 0 {
		o.sampleInterval = 30 * time.Second
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reg := metrics.NewRegistry()
	// build_info is a constant level for the lifetime of the process.
	reg.SetGauge("llz_reconcile_build_info", "llz reconciler build info (constant 1)",
		map[string]string{"version": version}, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = reg.WriteTo(w)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{Addr: o.metricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()

	// Sample once immediately so metrics are populated before the first scrape,
	// then on every tick.
	sample(ctx, client, reg, time.Now())
	ticker := time.NewTicker(o.sampleInterval)
	defer ticker.Stop()

	for {
		select {
		case err := <-errc:
			return fmt.Errorf("metrics server: %w", err)
		case <-ctx.Done():
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutCtx)
		case <-ticker.C:
			sample(ctx, client, reg, time.Now())
		}
	}
}

// sample performs one observe-only pass and updates the registry. Any API error
// sets llz_reconcile_up to 0 (and leaves the prior level gauges in place, so a
// transient blip does not erase the last-known-good sample); a success sets it
// to 1. last_sample_timestamp_seconds always advances so a staleness alert can
// distinguish "reconciler wedged" from "reconciler reporting failures".
func sample(ctx context.Context, client nodeGetter, reg *metrics.Registry, now time.Time) {
	reg.SetGauge("llz_reconcile_last_sample_timestamp_seconds",
		"unix time of the reconciler's most recent sample attempt", nil, float64(now.Unix()))

	obj, status, err := client.GetJSON(ctx, "/api/v1/nodes")
	if err != nil || status < 200 || status >= 300 || obj == nil {
		reg.SetGauge("llz_reconcile_up", "1 if the reconciler's last sample succeeded", nil, 0)
		return
	}
	ready, total := tallyNodeReadiness(obj)
	reg.SetGauge("llz_reconcile_nodes_ready", "count of Nodes with Ready=True", nil, float64(ready))
	reg.SetGauge("llz_reconcile_nodes_total", "count of Nodes", nil, float64(total))
	reg.SetGauge("llz_reconcile_up", "1 if the reconciler's last sample succeeded", nil, 1)
}

// tallyNodeReadiness counts Nodes and those whose Ready condition is True, reading
// the subset of a NodeList (`/api/v1/nodes`) the signal needs. It is defensive
// against missing/oddly-typed fields — a malformed item counts toward total but
// not ready, never panics.
func tallyNodeReadiness(list map[string]any) (ready, total int) {
	items, _ := list["items"].([]any)
	for _, it := range items {
		total++
		node, ok := it.(map[string]any)
		if !ok {
			continue
		}
		status, ok := node["status"].(map[string]any)
		if !ok {
			continue
		}
		conds, ok := status["conditions"].([]any)
		if !ok {
			continue
		}
		for _, c := range conds {
			cond, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if cond["type"] == "Ready" && cond["status"] == "True" {
				ready++
				break
			}
		}
	}
	return ready, total
}
