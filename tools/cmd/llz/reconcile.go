// `llz reconcile` is the in-cluster reconciler process (see
// docs/designs/kube-native-reconciler.md). It is the long-lived counterpart to
// the `ci <verb>` one-shots the polling CronJobs run today: instead of a fixed-
// interval CronJob reaching in, one leader-elected Deployment watches cluster
// state and exposes a Prometheus metrics surface so the cluster self-reports.
//
// The always-on `observe` reconciler drives nothing (it samples cluster signals
// and publishes them at :8080/metrics). The optional reconcilers each replace a
// CronJob and stay OFF by default — the watch-driven argo-nudge (Phase 1) and the
// timed linode-creds / harbor (Phase 2). It uses internal/kube (the hand-rolled
// REST client — no client-go) and internal/metrics (hand-rolled text exposition —
// no prometheus/client_golang), staying inside the module's deliberately-lean
// dependency stance.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
			"  --reconcile-argo-nudge     re-trigger terminally-failed Argo Applications, watch-\n" +
			"                             driven (was the argo-resync-nudger CronJob)\n" +
			"  --reconcile-cidr-firewall  reconcile the CIDR-firewall ConfigMap on Node change\n" +
			"                             (was the cidrFirewall CronJob; needs NODE_NAME,\n" +
			"                             LINODE_TOKEN)\n" +
			"  --reconcile-volume-labels  rename Linode Volumes for bound PVs, watch-driven (was\n" +
			"                             the linode-volume-labeler CronJob; needs REGION_SHORT,\n" +
			"                             LINODE_TOKEN)\n" +
			"  --reconcile-linode-creds   rotate in-cluster Linode object-storage keys (was\n" +
			"                             the linodeCredRotator CronJob; needs REGION,\n" +
			"                             OBJ_CLUSTER, LINODE_TOKEN, OPENBAO_*)\n" +
			"  --reconcile-harbor         ensure Harbor project + robots (was the\n" +
			"                             harbor-robot-provisioner CronJob)\n" +
			"  --reconcile-openbao-gauges seal + credential-age gauges (read-only; needs\n" +
			"                             OpenBao egress + the reconciler k8s-auth role)\n" +
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
				leaderElection:      o.leaderElection,
				reconcileArgoNudge:  o.reconcileArgoNudge,
				argoNudgeResync:     time.Duration(o.argoNudgeResync) * time.Second,
				reconcileCidrFW:     o.reconcileCidrFW,
				cidrFWResync:        time.Duration(o.cidrFWResync) * time.Second,
				reconcileVolLabels:  o.reconcileVolLabels,
				volLabelsResync:     time.Duration(o.volLabelsResync) * time.Second,
				reconcileLinodeCred: o.reconcileLinodeCred,
				linodeCredInterval:  time.Duration(o.linodeCredInterval) * time.Second,
				reconcileHarbor:     o.reconcileHarbor,
				harborInterval:      time.Duration(o.harborInterval) * time.Second,
				reconcileOpenBao:    o.reconcileOpenBao,
				openbaoInterval:     time.Duration(o.openbaoInterval) * time.Second,
			})
		},
	}
	f := c.Flags()
	f.StringVar(&o.metricsAddr, "metrics-addr", ":8080", "address to serve the Prometheus /metrics endpoint on")
	f.IntVar(&o.sampleInterval, "sample-interval", 30, "seconds between observe-reconciler cluster samples")
	f.BoolVar(&o.leaderElection, "leader-election", true, "gate the driving reconcilers on a coordination.k8s.io Lease (single writer)")
	f.BoolVar(&o.reconcileArgoNudge, "reconcile-argo-nudge", false, "enable the argo-resync-nudger watch reconciler (default off: the CronJob owns it)")
	f.IntVar(&o.argoNudgeResync, "argo-nudge-resync", 300, "resync-floor seconds for the argo-nudge reconciler (watch drives the immediacy)")
	f.BoolVar(&o.reconcileCidrFW, "reconcile-cidr-firewall", false, "enable the CIDR-firewall discovery watch reconciler (default off: the CronJob owns it)")
	f.IntVar(&o.cidrFWResync, "cidr-firewall-resync", 600, "resync-floor seconds for the cidr-firewall reconciler (Node watch drives immediacy)")
	f.BoolVar(&o.reconcileVolLabels, "reconcile-volume-labels", false, "enable the Linode Volume relabeler watch reconciler (default off: the CronJob owns it)")
	f.IntVar(&o.volLabelsResync, "volume-labels-resync", 3600, "resync-floor seconds for the volume-labels reconciler (PV watch drives immediacy)")
	f.BoolVar(&o.reconcileLinodeCred, "reconcile-linode-creds", false, "enable the Linode credential-rotation reconciler (default off: the CronJob owns it)")
	f.IntVar(&o.linodeCredInterval, "linode-creds-interval", 3600, "seconds between Linode credential-rotation resync passes")
	f.BoolVar(&o.reconcileHarbor, "reconcile-harbor", false, "enable the Harbor-provisioner reconciler (default off: the CronJob owns it)")
	f.IntVar(&o.harborInterval, "harbor-interval", 300, "seconds between Harbor-provisioner resync passes")
	f.BoolVar(&o.reconcileOpenBao, "reconcile-openbao-gauges", false, "enable the OpenBao seal + credential-age gauges (read-only; needs OpenBao egress + the reconciler k8s-auth role)")
	f.IntVar(&o.openbaoInterval, "openbao-gauges-interval", 60, "seconds between OpenBao gauge samples")
	return c
}

// reconcileFlags holds the raw flag values (seconds as ints); runReconcile takes
// the typed reconcileOpts.
type reconcileFlags struct {
	metricsAddr         string
	sampleInterval      int
	leaderElection      bool
	reconcileArgoNudge  bool
	argoNudgeResync     int
	reconcileCidrFW     bool
	cidrFWResync        int
	reconcileVolLabels  bool
	volLabelsResync     int
	reconcileLinodeCred bool
	linodeCredInterval  int
	reconcileHarbor     bool
	harborInterval      int
	reconcileOpenBao    bool
	openbaoInterval     int
}

type reconcileOpts struct {
	metricsAddr         string
	sampleInterval      time.Duration
	leaderElection      bool
	reconcileArgoNudge  bool
	argoNudgeResync     time.Duration
	reconcileCidrFW     bool
	cidrFWResync        time.Duration
	reconcileVolLabels  bool
	volLabelsResync     time.Duration
	reconcileLinodeCred bool
	linodeCredInterval  time.Duration
	reconcileHarbor     bool
	harborInterval      time.Duration
	reconcileOpenBao    bool
	openbaoInterval     time.Duration
}

// drivingEnabled reports whether any state-mutating reconciler is on — the case
// that needs a single writer, hence leader election.
func (o reconcileOpts) drivingEnabled() bool {
	return o.reconcileArgoNudge || o.reconcileCidrFW || o.reconcileVolLabels ||
		o.reconcileLinodeCred || o.reconcileHarbor
}

// nodeGetter is the slice of the kube client the observe sampler needs.
type nodeGetter interface {
	GetJSON(ctx context.Context, path string) (map[string]any, int, error)
}

// reconcileClient is the full kube-client surface the reconcilers use (the observe
// sampler reads, the watch reconcilers also patch + watch). *kube.Client
// satisfies it; tests drive it with an httptest-backed real *kube.Client.
type reconcileClient interface {
	GetJSON(ctx context.Context, path string) (map[string]any, int, error)
	CreateJSON(ctx context.Context, path string, obj any) (int, error)
	MergePatch(ctx context.Context, path string, patch any) error
	Watch(ctx context.Context, path, resourceVersion string, fn func(kube.WatchEvent) error) error
}

// runReconcile serves the metrics endpoint and runs the reconciler manager until
// the context is cancelled (SIGTERM in a pod), then shuts the server down cleanly.
func runReconcile(ctx context.Context, client reconcileClient, o reconcileOpts) error {
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

	// Leader election: the observe reconciler is read-only and runs on every
	// replica, but the driving reconcilers need a single writer. gate wraps a
	// driving reconciler's run so it no-ops on a non-leader; without leader
	// election (or with none enabled) it is the identity.
	gate := func(run func(context.Context) error) func(context.Context) error { return run }
	if o.leaderElection && o.drivingEnabled() {
		elector := newLeaderElector(client, podNamespace(), "llz-reconciler-leader", podIdentity(), time.Now)
		gate = func(run func(context.Context) error) func(context.Context) error {
			return func(rctx context.Context) error {
				if !elector.IsLeader() {
					return nil // a peer holds the lease — do not drive
				}
				return run(rctx)
			}
		}
		go elector.run(ctx)
		go publishLeaderGauge(ctx, reg, elector)
	}

	// The manager blocks until ctx is cancelled; run it in the background so we
	// can also watch for a server error.
	done := make(chan struct{})
	go func() {
		runManager(ctx, reg, time.Now, buildReconcilers(reg, client, o, gate))
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
// sampler (read-only, never gated) plus each optional reconciler its flag turns
// on. gate wraps a driving reconciler so it no-ops on a non-leader.
func buildReconcilers(reg *metrics.Registry, client reconcileClient, o reconcileOpts, gate func(func(context.Context) error) func(context.Context) error) []reconciler {
	if o.sampleInterval <= 0 {
		o.sampleInterval = 30 * time.Second
	}
	recs := []reconciler{{
		name:     "observe",
		interval: o.sampleInterval,
		// Read-only: node readiness + the convergence gauge (Argo app health via
		// the shared internal/health predicate). A convergence read failure zeroes
		// observe's up; a hard-failed cluster does not (that's a valid observation).
		run: func(ctx context.Context) error {
			if err := sampleNodes(ctx, client, reg); err != nil {
				return err
			}
			if err := sampleConvergence(ctx, client, reg); err != nil {
				return err
			}
			return sampleHealth(ctx, client, reg)
		},
	}}
	if o.reconcileArgoNudge {
		recs = append(recs, reconciler{
			name:     "argo-nudge",
			interval: o.argoNudgeResync, // resync floor; the watch drives immediacy
			run:      gate(func(ctx context.Context) error { return reconcileArgoNudge(ctx, client) }),
			watch: func(ctx context.Context, onEvent func()) error {
				return client.Watch(ctx, argoAppsPath, "", func(kube.WatchEvent) error {
					onEvent()
					return nil
				})
			},
		})
	}
	if o.reconcileCidrFW {
		recs = append(recs, reconciler{
			name:     "cidr-firewall",
			interval: o.cidrFWResync,
			// Same logic the cidrFirewall CronJob runs (`ci discover-firewall-config`);
			// reads NODE_NAME/LINODE_TOKEN from env. Node changes shift which instance
			// (and thus which firewall/VPC) backs this pod, so watch Nodes.
			run: gate(func(ctx context.Context) error { return runCIDiscoverFirewallConfig(ctx) }),
			watch: func(ctx context.Context, onEvent func()) error {
				return client.Watch(ctx, "/api/v1/nodes", "", func(kube.WatchEvent) error {
					onEvent()
					return nil
				})
			},
		})
	}
	if o.reconcileVolLabels {
		recs = append(recs, reconciler{
			name:     "volume-labels",
			interval: o.volLabelsResync,
			// Go port of the linode-volume-labeler relabel.sh (`ci relabel-volumes`);
			// reads REGION_SHORT/LINODE_TOKEN from env. A new PV means a new Linode
			// Volume to relabel, so watch PersistentVolumes.
			run: gate(func(ctx context.Context) error { return runRelabelVolumes(ctx) }),
			watch: func(ctx context.Context, onEvent func()) error {
				return client.Watch(ctx, "/api/v1/persistentvolumes", "", func(kube.WatchEvent) error {
					onEvent()
					return nil
				})
			},
		})
	}
	if o.reconcileLinodeCred {
		recs = append(recs, reconciler{
			name:     "linode-creds",
			interval: o.linodeCredInterval,
			// Same logic the linodeCredRotator CronJob runs (`ci rotate-linode-creds
			// --apply`); reads REGION/OBJ_CLUSTER/LINODE_TOKEN/OPENBAO_* from env.
			run: gate(func(ctx context.Context) error { return runRotateLinodeCreds(ctx, true) }),
		})
	}
	if o.reconcileHarbor {
		recs = append(recs, reconciler{
			name:     "harbor",
			interval: o.harborInterval,
			// Same logic the harbor-robot-provisioner CronJob runs.
			run: gate(func(context.Context) error { return runCIHarborProvisioner() }),
		})
	}
	if o.reconcileOpenBao {
		recs = append(recs, reconciler{
			name:     "openbao-gauges",
			interval: o.openbaoInterval,
			// Read-only (seal + credential-age gauges), so NOT gated on leadership
			// — every replica may read OpenBao harmlessly.
			run: func(ctx context.Context) error { return sampleOpenBao(ctx, reg, time.Now()) },
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

// publishLeaderGauge mirrors the elector's leadership into a metric (1 on the
// leader, 0 on standbys) so a scrape shows which replica is driving. Updates on a
// short interval until ctx is cancelled.
func publishLeaderGauge(ctx context.Context, reg *metrics.Registry, e *leaderElector) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	set := func() {
		v := 0.0
		if e.IsLeader() {
			v = 1
		}
		reg.SetGauge("llz_reconcile_leader", "1 if this replica currently holds the reconciler lease", nil, v)
	}
	set()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			set()
		}
	}
}

// podIdentity is the leader-election holder identity: the pod name (unique per
// replica) from the downward-API POD_NAME, falling back to the hostname.
func podIdentity() string {
	if n := os.Getenv("POD_NAME"); n != "" {
		return n
	}
	h, _ := os.Hostname()
	return h
}

// podNamespace is the namespace the Lease lives in: the downward-API
// POD_NAMESPACE, then the mounted ServiceAccount namespace, then a sane default.
func podNamespace() string {
	if n := os.Getenv("POD_NAMESPACE"); n != "" {
		return n
	}
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return "llz-reconciler"
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
