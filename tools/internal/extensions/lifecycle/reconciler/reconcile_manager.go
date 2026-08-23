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
// proves out per-env (the design's "keep the CronJob until one color.Green e2e cycle"),
// so enabling one is an opt-in flag + the same env/secrets its CronJob had.
package reconciler

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"
)

// reconciler is one named reconcile loop. If watch is nil it runs purely on its
// interval (the timed reconcilers); if watch is set it is event-triggered (the
// Phase 1 watch reconcilers), with interval serving as a resync floor.
type reconciler struct {
	name string
	// ext and binding name the declaration this lane IS: the extension that
	// declares it and the Binding.Name of its `invariant:operating` binding.
	//
	// ────────────────────────────────────────────────────────────────────────
	// THE RECONCILER WAS A THIRD REGISTRY, AND IT HAD ALREADY DRIFTED.
	//
	// Gates dispatch from the extension registry and the assert battery is checked
	// against it. This scheduler was the last of the four binding kinds with no
	// relationship to the declarations at all — eleven lanes here, fifteen
	// invariant bindings there, and nothing comparing them. It could not: the
	// registry IMPORTS this package to declare it, so a call the other way is a
	// cycle.
	//
	// The drift was already real when this was added. Two lanes were declared
	// under a DIFFERENT name than they run under (`apl-overlay` was declared
	// `apl-overlay-sync`, `token-inventory` was declared `expiry-inventory`), and
	// two more ran with no declaration naming them at all. Most of the table
	// matched, which is exactly why nobody noticed the rest.
	//
	// Naming the declaration HERE rather than in the registry keeps the import
	// direction intact: this is data a test in internal/cli reads through
	// ReconcilerLanes(), which is the one package that can see both.
	// ────────────────────────────────────────────────────────────────────────
	ext      string
	binding  string
	interval time.Duration
	// run performs one reconcile pass. It must be idempotent and safe to call
	// repeatedly (level-based: it re-reads state and acts, so a pass with nothing
	// to do is a no-op). A non-nil error marks the pass failed (up=0, errors_total++).
	run func(ctx context.Context) error
	// watch, if set, makes this reconciler event-triggered: the manager keeps the
	// stream open (re-establishing on close) and calls onEvent per frame; each
	// event (plus every interval as a resync floor, plus a reconnect catch-up)
	// runs one `run` pass. watch must honour ctx (return when it is cancelled).
	watch func(ctx context.Context, onEvent func()) error
}

// watchReconnectBackoff is the pause before re-establishing a dropped watch
// stream, so a persistently-failing watch does not hot-loop. A package var so
// tests can shrink it.
//
// FLAT, IT DID NOT PREVENT THE HOT LOOP IT NAMES. A watch that fails IMMEDIATELY —
// RBAC denied on the collection, the CRD version gone, a field selector the
// apiserver rejects — returns at once, the catch-up fire() runs a FULL RELIST, and
// one second later it all happens again. For the life of the pod: roughly 1Hz of
// list traffic against the apiserver, with llz_reconcile_up pinned at 1 the whole
// time, because the reconcile passes are succeeding. It is the watch that is dead,
// and nothing said so — the error was discarded with `_ =`.
var watchReconnectBackoff = time.Second

// watchReconnectBackoffMax caps the exponential retreat. One minute keeps a
// recovering watch responsive while costing a permanently broken one ~60 relists
// an hour instead of ~3,600; the reconciler's own resync interval is what keeps
// correctness while the watch is down, so backing off loses nothing but load.
var watchReconnectBackoffMax = time.Minute

// watchBackoffWait is the seam the reconnect loop actually pauses on. It exists
// so a test can observe the DURATIONS THE LOOP ASKS FOR — asserting that
// watchBackoffFor is exponential proves nothing about whether the loop calls it,
// and a first cut of that test stayed green while the call site was reverted to
// the flat constant.
var watchBackoffWait = func(d time.Duration) <-chan time.Time { return time.After(d) }

// watchBackoffFor returns the pause after n consecutive watch failures: the base
// doubled per failure, capped. n == 0 (the watch ran and closed cleanly) is the
// base, which is the old behaviour for the case it was written for.
func watchBackoffFor(n int) time.Duration {
	d := watchReconnectBackoff
	for i := 0; i < n && d < watchReconnectBackoffMax; i++ {
		d *= 2
	}
	if d > watchReconnectBackoffMax {
		d = watchReconnectBackoffMax
	}
	return d
}

// kicker fans a one-shot "reconcile now" signal out to every manager loop. It lets
// leader-election acquisition (leaderElector.onAcquire) re-run the gated reconcilers
// immediately rather than leaving them no-op'd until their next resync floor — the
// cold-start race that left two default StorageClasses standing for ~120s. Each loop
// gets its own size-1 coalescing subscription; Kick does a non-blocking send to all,
// so a burst collapses to one pending pass and Kick never blocks its caller.
type kicker struct {
	mu   sync.Mutex
	subs []chan struct{}
}

// subscribe registers a new loop and returns its kick channel. Called synchronously
// from runManager as it spawns loops, so every loop is subscribed before the elector
// can fire its first Kick.
func (k *kicker) subscribe() <-chan struct{} {
	ch := make(chan struct{}, 1)
	k.mu.Lock()
	k.subs = append(k.subs, ch)
	k.mu.Unlock()
	return ch
}

// Kick requests one extra reconcile pass on every subscribed loop. Non-blocking,
// safe for concurrent use, and a no-op on a nil *kicker.
func (k *kicker) Kick() {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, ch := range k.subs {
		select {
		case ch <- struct{}{}:
		default: // a kick is already pending on this loop — coalesce
		}
	}
}

// runManager runs every reconciler concurrently until ctx is cancelled, then
// waits for their loops to exit. now is injected so tests drive time; production
// passes time.Now. k, when non-nil, lets an external signal (leader acquisition)
// re-trigger every loop immediately; pass nil to disable.
func runManager(ctx context.Context, reg *metrics.Registry, now func() time.Time, recs []reconciler, k *kicker) {
	var wg sync.WaitGroup
	for _, r := range recs {
		wg.Add(1)
		var kick <-chan struct{}
		if k != nil {
			kick = k.subscribe()
		}
		go func(r reconciler, kick <-chan struct{}) {
			defer wg.Done()
			if r.watch != nil {
				runWatchReconcilerLoop(ctx, reg, now, r, kick)
			} else {
				runReconcilerLoop(ctx, reg, now, r, kick)
			}
		}(r, kick)
	}
	wg.Wait()
}

// runWatchReconcilerLoop runs r level-based: an initial pass, then one pass per
// trigger — a watch event, a resync-floor tick, or a reconnect catch-up. The
// watch is a notification channel only (missed events are covered by the resync
// floor and the re-list every pass does), so bursts coalesce into a single
// pending reconcile via the size-1 trigger channel.
func runWatchReconcilerLoop(ctx context.Context, reg *metrics.Registry, now func() time.Time, r reconciler, kick <-chan struct{}) {
	trigger := make(chan struct{}, 1)
	fire := func() {
		select {
		case trigger <- struct{}{}:
		default: // a reconcile is already pending — coalesce
		}
	}

	// Watcher goroutine: keep a stream open, firing per event; on any close (clean
	// or error) fire a catch-up pass and re-establish after a backoff.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		lbl := map[string]string{"reconciler": r.name}
		fails := 0
		for ctx.Err() == nil {
			err := r.watch(ctx, fire)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				fails++
				reg.AddCounter("llz_watch_errors_total",
					"total watch-stream failures per reconciler", lbl, 1)
				reg.SetGauge("llz_watch_connected",
					"1 if the reconciler's watch stream last closed cleanly", lbl, 0)
				// Logged on the first failure and then on powers of two, so a
				// permanently broken watch is visible without burying every other
				// line in the pod's log. Discarding it entirely — which is what
				// `_ = r.watch(...)` did — left an RBAC denial looking exactly like
				// a healthy stream that happened to close.
				if fails == 1 || fails&(fails-1) == 0 {
					log.Printf("reconcile %q: watch failed (%d consecutive): %v", r.name, fails, err)
				}
			} else {
				if fails > 0 {
					log.Printf("reconcile %q: watch re-established after %d failure(s)", r.name, fails)
				}
				fails = 0
				reg.SetGauge("llz_watch_connected",
					"1 if the reconciler's watch stream last closed cleanly", lbl, 1)
			}
			fire() // stream dropped mid-run — reconcile to catch anything missed
			select {
			case <-ctx.Done():
				return
			case <-watchBackoffWait(watchBackoffFor(fails)):
			}
		}
	}()

	reconcileOnce(ctx, reg, now, r) // initial pass

	var tick <-chan time.Time
	if r.interval > 0 {
		t := time.NewTicker(r.interval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-trigger:
			reconcileOnce(ctx, reg, now, r)
		case <-tick:
			reconcileOnce(ctx, reg, now, r)
		case <-kick:
			reconcileOnce(ctx, reg, now, r) // leader just acquired — re-run now, not at the next resync floor
		}
	}
}

// runReconcilerLoop runs r once immediately (so metrics populate before the first
// scrape) then on every interval tick — or on a kick (leader acquisition) — until
// ctx is cancelled. A nil tick (interval<=0) and/or nil kick simply never fire, so
// a cadence-less reconciler just holds until shutdown.
func runReconcilerLoop(ctx context.Context, reg *metrics.Registry, now func() time.Time, r reconciler, kick <-chan struct{}) {
	reconcileOnce(ctx, reg, now, r)
	var tick <-chan time.Time
	if r.interval > 0 {
		t := time.NewTicker(r.interval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			reconcileOnce(ctx, reg, now, r)
		case <-kick:
			reconcileOnce(ctx, reg, now, r) // leader just acquired — re-run now, not at the next resync floor
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
	// Publish the lane's own cadence so staleness can be alerted GENERICALLY —
	// "no successful pass in 10× your own interval" — instead of one rule
	// hard-coded to the observe lane's 30s. Every gauge this lane publishes is
	// upsert-only (the registry never expires a sample), so a lane that dies keeps
	// serving its last good values forever; its freshness series is the only thing
	// that can contradict them. A 0-interval lane is event-driven and has no
	// cadence to be stale against, so it publishes none.
	if r.interval > 0 {
		reg.SetGauge("llz_reconcile_interval_seconds",
			"configured interval between this reconciler's passes", lbl, r.interval.Seconds())
	}

	start := now()
	err := r.run(ctx)
	end := now()
	reg.SetGauge("llz_reconcile_last_duration_seconds", "wall-clock duration of the last reconcile pass",
		lbl, end.Sub(start).Seconds())

	if err != nil {
		// Log the failure to stderr (captured by `kubectl logs`). Without this the
		// only trace of a failing pass is up=0 / errors_total++ in the metrics —
		// undebuggable when the metric itself isn't reaching Prometheus, and a dead
		// end even when it is (the counter says THAT it failed, never WHY). e.g. the
		// openbao-gauges k8s-auth failure was invisible until this line existed.
		log.Printf("reconcile %q failed: %v", r.name, err)
		reg.AddCounter("llz_reconcile_errors_total", "total failed reconcile passes per reconciler", lbl, 1)
		reg.SetGauge("llz_reconcile_up", "1 if the reconciler's last pass succeeded", lbl, 0)
		return
	}
	reg.SetGauge("llz_reconcile_up", "1 if the reconciler's last pass succeeded", lbl, 1)
	reg.SetGauge("llz_reconcile_last_success_timestamp_seconds",
		"unix time of the reconciler's most recent successful pass", lbl, float64(end.Unix()))
}

// LaneDecl is one runtime lane's declaration attribution, for the coupling test.
type LaneDecl struct {
	// Lane is the name the scheduler and the metrics use.
	Lane string
	// Extension and Binding are the declaration this lane IS.
	Extension string
	Binding   string
}

// ReconcilerLanes reports every lane this binary can schedule, with the
// declaration each one claims.
//
// IT BUILDS THE FULL SET, not the enabled one: the coupling it feeds is between
// the CODE and the MODEL, and a lane behind a disabled flag is still a lane whose
// declaration must exist. reconcileOpts is set to all-on for that reason.
//
// Exported for internal/cli's coupling test — the registry cannot call it (it
// imports this package to declare it), and this package cannot call the registry
// for the same reason. See the ext/binding fields on reconciler.
func ReconcilerLanes() []LaneDecl {
	all := reconcileOpts{
		reconcileArgoNudge:  true,
		reconcileCidrFW:     true,
		reconcileVolLabels:  true,
		reconcileVolTags:    true,
		reconcileSCDemote:   true,
		reconcileLinodeCred: true,
		reconcileOpenBao:    true,
		reconcileTokens:     true,
		reconcileESRecovery: true,
		reconcileAplOverlay: true,
	}
	var out []LaneDecl
	for _, r := range buildReconcilers(nil, nil, all, func(f func(context.Context) error) func(context.Context) error { return f }) {
		out = append(out, LaneDecl{Lane: r.name, Extension: r.ext, Binding: r.binding})
	}
	return out
}
