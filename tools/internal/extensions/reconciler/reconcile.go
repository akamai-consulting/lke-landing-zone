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
package reconciler

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/volumes"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/reconcilelanes"
)

// reconcileFlags holds the raw flag values (seconds as ints); runReconcile takes
// the typed reconcileOpts.
type reconcileFlags struct {
	metricsAddr         string
	healthAddr          string
	metricsCertFile     string
	metricsKeyFile      string
	metricsClientCAFile string
	sampleInterval      int
	leaderElection      bool
	reconcileArgoNudge  bool
	argoNudgeResync     int
	reconcileCidrFW     bool
	cidrFWResync        int
	reconcileVolLabels  bool
	volLabelsResync     int
	reconcileVolTags    bool
	volTagsResync       int
	reconcileLinodeCred bool
	linodeCredInterval  int
	reconcileOpenBao    bool
	openbaoInterval     int
	reconcileTokens     bool
	tokensInterval      int
	reconcileSCDemote   bool
	scDemoteResync      int
	scDemoteName        string
	reconcileESRecovery bool
	esRecoveryResync    int
	reconcileAplOverlay bool
	aplOverlayInterval  int
}

type reconcileOpts struct {
	metricsAddr         string
	healthAddr          string
	metricsCertFile     string
	metricsKeyFile      string
	metricsClientCAFile string
	sampleInterval      time.Duration
	leaderElection      bool
	reconcileArgoNudge  bool
	argoNudgeResync     time.Duration
	reconcileCidrFW     bool
	cidrFWResync        time.Duration
	reconcileVolLabels  bool
	volLabelsResync     time.Duration
	reconcileVolTags    bool
	volTagsResync       time.Duration
	reconcileSCDemote   bool
	scDemoteResync      time.Duration
	scDemoteName        string
	reconcileLinodeCred bool
	linodeCredInterval  time.Duration
	reconcileOpenBao    bool
	openbaoInterval     time.Duration
	reconcileTokens     bool
	tokensInterval      time.Duration
	reconcileESRecovery bool
	esRecoveryResync    time.Duration
	reconcileAplOverlay bool
	aplOverlayInterval  time.Duration
}

// openbaoBootstrapGrace wraps the read-only openbao-gauges sample so an
// unreachable OpenBao is a clean no-op UNTIL it has answered once, then a
// real error thereafter. The reconciler now starts at wave 0 (before OpenBao
// is up — secrets-before-apps Phase 2), so without this a fresh bootstrap
// would accrue llz_reconcile_errors_total and trip the OpenBao-down alerts for
// ~10-15m every time; a genuine day-2 outage (after the store was once up)
// still surfaces. Per-process sticky bool (replicas=1, lane is un-gated).
func openbaoBootstrapGrace(sample func(context.Context) error) func(context.Context) error {
	seen := false
	return func(ctx context.Context) error {
		err := sample(ctx)
		if err == nil {
			seen = true
			return nil
		}
		if !seen {
			return nil // pre-OpenBao bootstrap window — expected, not a fault
		}
		return err
	}
}

// drivingEnabled reports whether any state-mutating reconciler is on — the case
// that needs a single writer, hence leader election.
func (o reconcileOpts) drivingEnabled() bool {
	return o.reconcileArgoNudge || o.reconcileCidrFW || o.reconcileVolLabels || o.reconcileVolTags ||
		o.reconcileSCDemote || o.reconcileLinodeCred ||
		o.reconcileESRecovery || o.reconcileAplOverlay
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

// reconcilerHealthz is the /healthz handler and the liveness probe's target. With
// leader election active it returns 503 once the elector loop has wedged (no
// evaluation within staleAfter), so kubelet restarts a pod whose leader election
// has silently died — the failure mode where the pod stays Running+Ready while
// llz_reconcile_leader is stuck at 0 and every driving reconciler no-ops. It does
// NOT fail merely because this replica is a non-leader: a legitimate standby is
// healthy, and a nil elector (election disabled) always reports ok.
func reconcilerHealthz(e *leaderElector) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if e != nil && !e.Healthy() {
			http.Error(w, "leader-election loop wedged (no progress within staleAfter)", http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintln(w, "ok")
	}
}

// runReconcile serves the metrics endpoint and runs the reconciler manager until
// the context is cancelled (SIGTERM in a pod), then shuts the server down cleanly.
func runReconcile(ctx context.Context, client reconcileClient, o reconcileOpts) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reg := metrics.NewRegistry()
	// build_info is a constant level for the lifetime of the process.
	reg.SetGauge("llz_reconcile_build_info", "llz reconciler build info (constant 1)",
		map[string]string{"version": Version}, 1)

	// Leader election: create the elector up-front (nil when disabled) so /healthz
	// can gate liveness on its progress. The observe reconciler is read-only and
	// runs on every replica; the driving reconcilers need a single writer, so gate
	// wraps them to no-op on a non-leader (the identity when election is off).
	var elector *leaderElector
	// kick lets the elector re-run every manager loop the instant it acquires the
	// Lease, so the gated reconcilers don't sit no-op'd until their next resync floor
	// after a cold start (the sc-demote two-defaults-for-~120s race). Harmless when
	// election is off: onAcquire is never wired, so Kick is never called.
	kick := &kicker{}
	gate := func(run func(context.Context) error) func(context.Context) error { return run }
	if o.leaderElection && o.drivingEnabled() {
		elector = newLeaderElector(client, podNamespace(), "llz-reconciler-leader", podIdentity(), time.Now)
		elector.onAcquire = kick.Kick
		gate = func(run func(context.Context) error) func(context.Context) error {
			return func(rctx context.Context) error {
				if !elector.IsLeader() {
					return nil // a peer holds the lease — do not drive
				}
				return run(rctx)
			}
		}
	}

	// TWO listeners, deliberately split by who can authenticate.
	//
	// /metrics moves onto an mTLS listener: Prometheus presents a client cert
	// from llz-client-ca and verifies this server against llz-serving-ca. It was
	// plaintext, scraped over `scheme: http`.
	//
	// /healthz STAYS PLAINTEXT on its own port, because the client is the KUBELET
	// running the liveness/readiness probes, and kubelet cannot present a client
	// certificate — an httpGet probe has no way to carry one. Requiring mTLS on a
	// single combined listener would make every probe fail and CrashLoop the pod.
	//
	// That split is safe because of what each endpoint returns: /healthz is a
	// leader-election liveness verdict with no cluster data in it, while /metrics
	// carries the llz_* gauge set. Keeping them on one port would have forced the
	// weaker of the two postures onto both.
	metricsMux := http.NewServeMux()
	metricsMux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = reg.WriteTo(w)
	})
	// /healthz doubles as the liveness probe: with leader election active it fails
	// once the elector loop wedges, so a silently-stuck reconciler is restarted
	// instead of sitting leaderless forever (see reconcilerHealthz).
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", reconcilerHealthz(elector))

	srv := &http.Server{Addr: o.metricsAddr, Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second}
	healthSrv := &http.Server{Addr: o.healthAddr, Handler: healthMux, ReadHeaderTimeout: 5 * time.Second}

	errc := make(chan error, 1)
	go func() {
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()
	go func() {
		// serveMetrics decides TLS-vs-plaintext from the flags; see it for why a
		// missing certificate is a hard error rather than a plaintext fallback.
		if err := serveMetrics(srv, o.metricsCertFile, o.metricsKeyFile, o.metricsClientCAFile); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()

	if elector != nil {
		go elector.run(ctx)
		go publishLeaderGauge(ctx, reg, elector)
	}

	// The manager blocks until ctx is cancelled; run it in the background so we
	// can also watch for a server error.
	done := make(chan struct{})
	go func() {
		runManager(ctx, reg, time.Now, buildReconcilers(reg, client, o, gate), kick)
		close(done)
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("metrics server: %w", err)
	case <-ctx.Done():
		<-done // let the reconciler loops unwind
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Both listeners now, not just the metrics one — a leaked health server
		// would hold the port and keep the process alive past shutdown.
		_ = healthSrv.Shutdown(shutCtx)
		return srv.Shutdown(shutCtx)
	}
}

// serveMetrics starts the /metrics listener, with mTLS when a serving keypair is
// configured and plaintext when it is not.
//
// The plaintext branch exists ONLY so `llz reconcile` stays runnable outside a
// cluster (local runs, tests) where no cert-manager has issued anything. In the
// deployed manifest all three paths are always set, so the in-cluster listener is
// always mTLS.
//
// A PARTIAL configuration is a hard error rather than a downgrade: if someone
// sets the cert but not the client CA, that is a server which encrypts but
// accepts any caller, and silently starting it would look exactly like success.
// The whole point of this change was removing a flag whose failure mode was a
// silent downgrade — reintroducing one here would be self-defeating.
func serveMetrics(srv *http.Server, certFile, keyFile, clientCAFile string) error {
	if certFile == "" && keyFile == "" && clientCAFile == "" {
		return srv.ListenAndServe()
	}
	if certFile == "" || keyFile == "" || clientCAFile == "" {
		return fmt.Errorf("metrics TLS is partially configured (cert=%q key=%q client-ca=%q): set all three or none — a serving cert without a client CA would accept unauthenticated scrapes",
			certFile, keyFile, clientCAFile)
	}
	caPEM, err := os.ReadFile(clientCAFile)
	if err != nil {
		return fmt.Errorf("read metrics client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("metrics client CA %s contains no valid certificate", clientCAFile)
	}
	// Fail fast on a missing/malformed keypair. With GetCertificate below the
	// files are otherwise not touched until the first scrape, which would turn a
	// bad mount into a silent "listener up, every scrape fails" state.
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return fmt.Errorf("metrics serving keypair (%s / %s): %w", certFile, keyFile, err)
	}
	srv.TLSConfig = &tls.Config{
		ClientCAs: pool,
		// The scrape must PROVE it is Prometheus, not merely offer a cert.
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS12,
		// GetCertificate re-reads the SERVING keypair per handshake, for the same
		// reason the client side uses GetClientCertificate: this process is a
		// long-lived Deployment and the leaf is 90 days. ListenAndServeTLS(cert,
		// key) loads once at startup, so after cert-manager renewed at day 60 the
		// server would keep serving the original leaf until it expired at day 90
		// and then fail every scrape — with nothing to restart it, since the
		// liveness probe is on the separate plaintext health port.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			c, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return nil, fmt.Errorf("reload metrics serving keypair: %w", err)
			}
			return &c, nil
		},
	}
	// Empty strings: the keypair comes from GetCertificate above, not from these
	// arguments. Passing the paths here would ALSO load them once at startup and
	// silently win for connections that do not hit GetCertificate.
	return srv.ListenAndServeTLS("", "")
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
			// Cheap presence gauge for the lazily-read in-cluster Linode token
			// (optional Secret volume — see linode_token.go): lets the e2e and
			// day-2 alerting see the linode lanes' activation state instead of
			// inferring it from their silence.
			present := 0.0
			if credrotate.InClusterLinodeToken() != "" {
				present = 1
			}
			reg.SetGauge("llz_reconcile_linode_token_present", "1 once the in-cluster Linode token (env or optional Secret volume) is readable", nil, present)
			if err := sampleNodes(ctx, client, reg); err != nil {
				return err
			}
			if err := sampleConvergence(ctx, client, reg); err != nil {
				return err
			}
			return sampleHealth(ctx, client, reg)
		},
	}}
	// requireLinodeToken wraps a linode-dependent lane: before the ESO-synced
	// token exists (bootstrap pre-store, by design since the Deployment's secret
	// ref became an optional volume) the lane is a clean no-op instead of a
	// failed pass — the observe gauge above reports the waiting state.
	requireLinodeToken := func(run func(context.Context) error) func(context.Context) error {
		return func(ctx context.Context) error {
			if credrotate.InClusterLinodeToken() == "" {
				return nil
			}
			return run(ctx)
		}
	}
	// …and the other half of that contract: re-kick the lane WHEN the token shows
	// up. Without this, the clean no-op above is a permanent one for anything whose
	// watch events all fired during bootstrap — see withLinodeTokenWait.
	linodeWatch := func(inner func(context.Context, func()) error) func(context.Context, func()) error {
		return withLinodeTokenWait(inner)
	}
	if o.reconcileArgoNudge {
		recs = append(recs, reconciler{
			name:     "argo-nudge",
			interval: o.argoNudgeResync, // resync floor; the watch drives immediacy
			run:      gate(func(ctx context.Context) error { return reconcilelanes.ArgoNudge(ctx, client) }),
			watch: func(ctx context.Context, onEvent func()) error {
				return client.Watch(ctx, reconcilelanes.ArgoAppsPath, "", func(kube.WatchEvent) error {
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
			run: gate(requireLinodeToken(func(ctx context.Context) error { return runCIDiscoverFirewallConfig(ctx) })),
			// Same first-boot gap as volume-labels: nodes settle long before the
			// token is seeded, so the arrival kick is what makes the first pass run.
			watch: linodeWatch(func(ctx context.Context, onEvent func()) error {
				return client.Watch(ctx, "/api/v1/nodes", "", func(kube.WatchEvent) error {
					onEvent()
					return nil
				})
			}),
		})
	}
	if o.reconcileVolLabels {
		recs = append(recs, reconciler{
			name:     "volume-labels",
			interval: o.volLabelsResync,
			// Go port of the linode-volume-labeler relabel.sh (`ci relabel-volumes`);
			// reads REGION_SHORT/LINODE_TOKEN from env. A new PV means a new Linode
			// Volume to relabel, so watch PersistentVolumes.
			run: gate(requireLinodeToken(func(ctx context.Context) error { return runRelabelVolumes(ctx) })),
			// linodeWatch: on a cold bootstrap every PV event fires BEFORE the token
			// is seeded, so without the token-arrival kick these Volumes keep their
			// pvc-<uuid> labels until the 1h resync floor. See withLinodeTokenWait.
			watch: linodeWatch(func(ctx context.Context, onEvent func()) error {
				return client.Watch(ctx, "/api/v1/persistentvolumes", "", func(kube.WatchEvent) error {
					onEvent()
					return nil
				})
			}),
		})
	}
	if o.reconcileVolTags {
		recs = append(recs, reconciler{
			name:     "volume-tags",
			interval: o.volTagsResync,
			// Tag-heal BACKSTOP (`ci reconcile-volume-tags`). The block-storage-retain
			// StorageClass stamps the desired tags — this cluster's lke<id> ownership tag
			// included — inside the CreateVolume call, so this lane exists only for
			// Volumes born untagged (a clone/snapshot PVC admitted while admission
			// control was degraded). Same PV watch as volume-labels: a new PV means a new
			// Volume whose tags need checking. Reads the desired set from the live
			// StorageClass, so it needs no per-env config — only LINODE_TOKEN.
			run: gate(requireLinodeToken(func(ctx context.Context) error {
				return runCIReconcileVolumeTags(ctx, volumes.DefaultTagsSC)
			})),
			// Same first-boot gap as volume-labels — see withLinodeTokenWait.
			watch: linodeWatch(func(ctx context.Context, onEvent func()) error {
				return client.Watch(ctx, "/api/v1/persistentvolumes", "", func(kube.WatchEvent) error {
					onEvent()
					return nil
				})
			}),
		})
	}
	if o.reconcileSCDemote {
		name := o.scDemoteName
		if name == "" {
			name = reconcilelanes.DefaultDemoteSC
		}
		recs = append(recs, reconciler{
			name:     "sc-demote",
			interval: o.scDemoteResync, // resync floor — defeats the admission-policy starvation
			run:      gate(func(ctx context.Context) error { return reconcilelanes.SCDemote(ctx, client, name) }),
			watch: func(ctx context.Context, onEvent func()) error {
				return client.Watch(ctx, reconcilelanes.SCStorageClassesPath, "", func(kube.WatchEvent) error {
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
			run: gate(requireLinodeToken(func(ctx context.Context) error { return credrotate.RunRotateLinodeCreds(ctx, true) })),
		})
	}
	if o.reconcileESRecovery {
		state := &reconcilelanes.ESStoreRecovery{}
		recs = append(recs, reconciler{
			name:     "es-store-recovery",
			interval: o.esRecoveryResync, // resync floor; the store watch drives immediacy
			// Driving (patches ES/PushSecret annotations) → leader-gated. State is
			// per-process; the leader gate means only the driving replica advances it.
			run: gate(func(ctx context.Context) error { return state.Reconcile(ctx, client, reg) }),
			watch: func(ctx context.Context, onEvent func()) error {
				return client.Watch(ctx, reconcilelanes.ESStoresWatchPath, "", func(kube.WatchEvent) error {
					onEvent()
					return nil
				})
			},
		})
	}
	if o.reconcileOpenBao {
		// Sticky bootstrap grace: since the reconciler now starts at wave 0 (before
		// OpenBao is up — secrets-before-apps Phase 2), an unreachable OpenBao on
		// the FIRST passes is expected, not a fault. Swallow the error until OpenBao
		// answers once, so a fresh cluster bootstrap doesn't accrue
		// llz_reconcile_errors_total{openbao-gauges} or trip the OpenBao-down alerts
		// for ~10-15m every time. After the first success a failure IS a real day-2
		// outage and surfaces normally (gauge + error + alert). Per-process bool;
		// replicas=1 and the lane is read-only/un-gated so no coordination needed.
		recs = append(recs, reconciler{
			name:     "openbao-gauges",
			interval: o.openbaoInterval,
			run:      openbaoBootstrapGrace(func(ctx context.Context) error { return reconcilelanes.SampleOpenBao(ctx, reg, time.Now()) }),
		})
	}
	if o.reconcileTokens {
		recs = append(recs, reconciler{
			name:     "token-inventory",
			interval: o.tokensInterval,
			// Read-only (re-exposes the token-inventory ConfigMap), so NOT gated —
			// every replica may read the ConfigMap harmlessly.
			run: func(ctx context.Context) error { return sampleTokenInventory(ctx, client, reg, time.Now()) },
		})
	}
	if o.reconcileAplOverlay {
		recs = append(recs, reconciler{
			name:     "apl-overlay",
			interval: o.aplOverlayInterval,
			// Git-syncs the apl-overlay onto apl-<env> with ff-retry. WRITES (to git),
			// so leader-gated — a single writer cooperating with apl-operator's pushes.
			// Reads GH_REPO/APL_VALUES_REPO_TOKEN/REGION + OpenBao from env.
			run: gate(func(ctx context.Context) error { return reconcileAplOverlay(ctx, reg) }),
			// Not a KUBE watch — the source is a remote git repo, so the interval stays
			// the steady-state cadence. What this levels on instead is the lane's
			// PRECONDITION: on a cold bootstrap the obj credential is seeded long after
			// the pod starts, and without a kick the obj.yaml push (and behind it Loki's
			// move onto S3, the measured converge tail) waits out the full resync floor.
			// Gives up after a bootstrap window, since an instance without object
			// storage never seeds it. See reconcile_apl_overlay_wait.go.
			watch: watchAplOverlayPrecondition,
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
