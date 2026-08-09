package reconciler

// reconcile_apl_overlay_wait.go closes the first-boot gap between the apl-overlay
// lane's only TRIGGER (its 300s resync floor) and its PRECONDITION (the obj
// platform credential existing in OpenBao).
//
// THE BUG. `llz ci converge`'s own long-pole report named this gate as the tail in
// 8 of 8 sampled release-e2e runs (30849319500 .. 30966772849) — 404-532s from the
// first poll, with every other check color.Green by poll 2. Run 30966772849, where the
// overlay's own commit timestamp on apl-e2e splits the wait from the work:
//
//	01:59:16  `llz ci mint-bootstrap-objkeys` seeds secret/obj/platform. Every
//	          apl-overlay pass before this printed "obj platform credential not
//	          seeded yet — skipping obj.yaml this pass" (overlay.Reconcile).
//	01:59:54  `llz ci converge` starts, already blocked on this one gate.
//	02:04:01  the next tick finally pushes obj.yaml    ← 285s of DEAD WAIT
//	02:06:09  apl-operator applies it                  ┐
//	02:06:26  ESO builds loki-s3-linode-credentials    ├ 237s of real work
//	02:07:58  Loki has re-rendered onto S3             ┘
//
// The credential lands at an arbitrary phase of the tick, so a full interval of dead
// wait is the EXPECTED cost, not a bad draw. The 237s downstream of the push is real
// work and stays.
//
// Having no watch is defensible on its face — the lane's source is a remote git repo,
// which emits no Kubernetes events. What makes it wrong here, and not for the other
// watch-less lanes, is that apl-overlay's work is gated on a ONE-TIME precondition
// that arrives after the pod starts: observe/openbao-gauges/token-inventory only
// sample read-only state, and linode-creds rotates on a genuinely time-based
// schedule, so for those a timer really is the whole trigger story.
//
// This is the same shape as the Linode-token gap that reconcile_linode_token_wait.go
// exists to close — a lane blind to its own precondition becoming satisfiable — and
// the fix is the same: watch for the precondition, kick the lane when it flips.
//
// Why not just lower the interval: --apl-overlay-interval is the steady-state
// cadence for a day-2 cluster whose overlay changes approximately never. Cutting it
// to 60s would multiply the lane's GitHub git-data traffic fivefold, forever, to
// catch a transition that happens once in a cluster's life.
//
// RESIDUAL, deliberately left to the interval: the kicked pass still no-ops if
// apl-<env> does not exist yet (overlay.ErrRefNotFound — apl-operator has not
// bootstrapped the branch). Measured ordering puts the branch ~5m ahead of the
// credential, and the resync floor remains the backstop for the inverted case.

import (
	"context"
	"log"
	"time"
)

// aplOverlayPreconditionPoll is how often the waiter re-checks, and
// aplOverlayPreconditionWindow is how long it keeps checking before giving up.
//
// The window is what makes the poll affordable. Unlike the Linode-token waiter's
// free file read, each check here is an OpenBao k8s-auth login + KV read, and the
// credential is NOT guaranteed to arrive: an instance that declares no
// objectStorage.cluster never seeds secret/obj/platform at all — a supported
// configuration (filesystem Loki; see checkLokiObjStorage), not a fault. Unbounded,
// this lane would poll OpenBao every 15s for the life of every such pod, forever.
//
// Bounded, the cost is at most one bootstrap window per pod start and then nothing,
// and giving up is safe by construction: the lane reverts to exactly the 300s resync
// floor it had before this file existed. 30m is far outside any observed
// pod-start → mint-bootstrap-objkeys gap (single-digit minutes on the release-e2e
// lane) while still capping a pathological instance at ~120 checks.
var (
	aplOverlayPreconditionPoll   = 15 * time.Second
	aplOverlayPreconditionWindow = 30 * time.Minute
)

// watchAplOverlayPrecondition is the apl-overlay lane's `watch`. The lane has no
// Kubernetes event source, so this stands in as one: it kicks the lane the moment
// the obj credential first becomes readable, then holds until ctx is cancelled.
//
// Holding is load-bearing, not idling — the manager treats a returning watch as a
// dropped stream and re-establishes it after a backoff, so returning once configreadiness.Satisfied
// would hot-loop the lane at the reconnect cadence forever.
func watchAplOverlayPrecondition(ctx context.Context, onEvent func()) error {
	waitForAplOverlayPreconditionThenKick(ctx, onEvent)
	<-ctx.Done()
	return ctx.Err()
}

// waitForAplOverlayPreconditionThenKick polls until the lane could actually do its
// work, fires onEvent once, and returns. Returns immediately WITHOUT kicking when
// the precondition already holds: that is the steady state on every pass after the
// first bootstrap, and the manager's own initial pass already covers it.
//
// Gives up after aplOverlayPreconditionWindow. Giving up is not a failure — an
// instance with no object storage never seeds the credential, and the lane's resync
// floor remains its trigger either way.
func waitForAplOverlayPreconditionThenKick(ctx context.Context, onEvent func()) {
	if aplOverlayPreconditionMet(ctx) {
		return
	}
	give := time.NewTimer(aplOverlayPreconditionWindow)
	defer give.Stop()
	t := time.NewTicker(aplOverlayPreconditionPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-give.C:
			// Say so once. Silence here is the difference between "this instance has
			// no object storage" and "the reconciler cannot reach OpenBao", and the
			// lane going quiet is otherwise indistinguishable from working.
			log.Printf("apl-overlay: obj credential still unreadable after %s — no longer watching for it; the lane keeps running on its resync floor", aplOverlayPreconditionWindow)
			return
		case <-t.C:
			if aplOverlayPreconditionMet(ctx) {
				// The transition this whole file exists for: without this kick the
				// obj.yaml push — and behind it Loki's move onto S3, the measured
				// converge tail — waits out the rest of the resync floor.
				onEvent()
				return
			}
		}
	}
}

// aplOverlayPreconditionMet reports whether an apl-overlay pass would do more than
// no-op: the ESO-synced repo token is present AND the obj platform credential is
// readable. Both are the exact conditions the pass itself skips on (see
// reconcileAplOverlay and overlay.Reconcile), so a true here means the kick lands on
// a pass with work to do.
//
// Every failure — unparseable env, OpenBao not yet serving (the reconciler starts at
// wave 0, well before it) — is simply "not met yet". This is a scheduling hint, not
// a health check; the lane's own pass owns the errors and the metrics.
func aplOverlayPreconditionMet(ctx context.Context) bool {
	cfg, err := aplOverlayConfigFromEnv()
	if err != nil || cfg.token == "" {
		return false
	}
	_, ok, err := readObjPlatformCreds(ctx, cfg.openbaoAddr)
	return err == nil && ok
}
