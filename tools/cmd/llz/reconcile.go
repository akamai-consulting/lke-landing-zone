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
	var o reconcileFlags
	c := &cobra.Command{
		Use:   "reconcile",
		Short: "run the in-cluster reconciler + Prometheus metrics surface",
		Long: "Long-lived in-cluster process that runs a set of reconcilers and serves a\n" +
			"Prometheus metrics surface at --metrics-addr/metrics. The observe reconciler\n" +
			"(always on) samples cluster convergence signals and drives nothing. The Phase 2\n" +
			"timed reconcilers are folded off their CronJobs and stay OFF by default — enable\n" +
			"one with its flag once it is ready to own the work per-env (it needs the same\n" +
			"env/secrets its CronJob had):\n" +
			"  --reconcile-linode-creds   rotate in-cluster Linode object-storage keys (was\n" +
			"                             the linodeCredRotator CronJob; needs REGION,\n" +
			"                             OBJ_CLUSTER, LINODE_TOKEN, OPENBAO_*)\n" +
			"  --reconcile-harbor         ensure Harbor project + robots (was the\n" +
			"                             harbor-robot-provisioner CronJob)\n" +
			"Runs from the slim distroless image with an in-pod ServiceAccount. Terminates\n" +
			"gracefully on SIGTERM.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := kube.NewInCluster()
			if err != nil {
				return err
			}
			return runReconcile(cmd.Context(), client, reconcileOpts{
				metricsAddr:         o.metricsAddr,
				sampleInterval:      time.Duration(o.sampleInterval) * time.Second,
				reconcileLinodeCred: o.reconcileLinodeCred,
				linodeCredInterval:  time.Duration(o.linodeCredInterval) * time.Second,
				reconcileHarbor:     o.reconcileHarbor,
				harborInterval:      time.Duration(o.harborInterval) * time.Second,
			})
		},
	}
	f := c.Flags()
	f.StringVar(&o.metricsAddr, "metrics-addr", ":8080", "address to serve the Prometheus /metrics endpoint on")
	f.IntVar(&o.sampleInterval, "sample-interval", 30, "seconds between observe-reconciler cluster samples")
	f.BoolVar(&o.reconcileLinodeCred, "reconcile-linode-creds", false, "enable the Linode credential-rotation reconciler (default off: the CronJob owns it)")
	f.IntVar(&o.linodeCredInterval, "linode-creds-interval", 3600, "seconds between Linode credential-rotation resync passes")
	f.BoolVar(&o.reconcileHarbor, "reconcile-harbor", false, "enable the Harbor-provisioner reconciler (default off: the CronJob owns it)")
	f.IntVar(&o.harborInterval, "harbor-interval", 300, "seconds between Harbor-provisioner resync passes")
	return c
}

// reconcileFlags holds the raw flag values (seconds as ints); runReconcile takes
// the typed reconcileOpts.
type reconcileFlags struct {
	metricsAddr         string
	sampleInterval      int
	reconcileLinodeCred bool
	linodeCredInterval  int
	reconcileHarbor     bool
	harborInterval      int
}

type reconcileOpts struct {
	metricsAddr         string
	sampleInterval      time.Duration
	reconcileLinodeCred bool
	linodeCredInterval  time.Duration
	reconcileHarbor     bool
	harborInterval      time.Duration
}

// nodeGetter is the slice of *kube.Client the sampler needs — narrowed to an
// interface so tests can drive it with a fake or an httptest-backed real client.
type nodeGetter interface {
	GetJSON(ctx context.Context, path string) (map[string]any, int, error)
}

// runReconcile serves the metrics endpoint and runs the reconciler manager until
// the context is cancelled (SIGTERM in a pod), then shuts the server down cleanly.
func runReconcile(ctx context.Context, client nodeGetter, o reconcileOpts) error {
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

	// The manager blocks until ctx is cancelled; run it in the background so we
	// can also watch for a server error.
	done := make(chan struct{})
	go func() {
		runManager(ctx, reg, time.Now, buildReconcilers(reg, client, o))
		close(done)
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("metrics server: %w", err)
	case <-ctx.Done():
		<-done // let the reconciler loops unwind
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

// buildReconcilers assembles the enabled reconciler set: the always-on observe
// sampler plus each Phase 2 timed reconciler its flag turns on.
func buildReconcilers(reg *metrics.Registry, client nodeGetter, o reconcileOpts) []reconciler {
	if o.sampleInterval <= 0 {
		o.sampleInterval = 30 * time.Second
	}
	recs := []reconciler{{
		name:     "observe",
		interval: o.sampleInterval,
		run:      func(ctx context.Context) error { return sampleNodes(ctx, client, reg) },
	}}
	if o.reconcileLinodeCred {
		recs = append(recs, reconciler{
			name:     "linode-creds",
			interval: o.linodeCredInterval,
			// Same logic the linodeCredRotator CronJob runs (`ci rotate-linode-creds
			// --apply`); reads REGION/OBJ_CLUSTER/LINODE_TOKEN/OPENBAO_* from env.
			run: func(ctx context.Context) error { return runRotateLinodeCreds(ctx, true) },
		})
	}
	if o.reconcileHarbor {
		recs = append(recs, reconciler{
			name:     "harbor",
			interval: o.harborInterval,
			// Same logic the harbor-robot-provisioner CronJob runs.
			run: func(context.Context) error { return runCIHarborProvisioner() },
		})
	}
	return recs
}

// sampleNodes is the observe reconciler's pass: it publishes the node-readiness
// gauges and returns an error on an API failure (which the manager records as
// up=0 without erasing the last-known-good node gauges — a transient blip does
// not blank the surface). The manager owns up / last-success / duration.
func sampleNodes(ctx context.Context, client nodeGetter, reg *metrics.Registry) error {
	obj, status, err := client.GetJSON(ctx, "/api/v1/nodes")
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 || obj == nil {
		return fmt.Errorf("GET /api/v1/nodes: status %d", status)
	}
	ready, total := tallyNodeReadiness(obj)
	reg.SetGauge("llz_reconcile_nodes_ready", "count of Nodes with Ready=True", nil, float64(ready))
	reg.SetGauge("llz_reconcile_nodes_total", "count of Nodes", nil, float64(total))
	return nil
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
