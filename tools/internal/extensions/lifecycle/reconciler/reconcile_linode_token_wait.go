package reconciler

// reconcile_linode_token_wait.go closes the first-boot gap between a
// linode-dependent reconciler lane's TRIGGERS and its PRECONDITION.
//
// THE BUG, measured on a release-e2e run:
//
//	18:26:09 .. 18:30:56  apl-core creates all 17 PVCs → PV watch fires → the
//	                      volume-labels lane runs → requireLinodeToken finds no
//	                      token and returns nil, a SILENT no-op.
//	18:31:53              `llz ci mint-bootstrap-pat` seeds
//	                      secret/linode/api-token; ESO syncs it ~1m later.
//	(never)               another PersistentVolume event.
//
// Every watch event the lane will ever receive fires BEFORE it is able to act, and
// nothing re-triggers it once the credential arrives. The lane's only remaining
// trigger is its resync floor — `--volume-labels-resync`, default 3600s — so a
// freshly bootstrapped cluster keeps the CSI's opaque `pvc-<uuid>` Volume labels for
// up to an hour. Observed on two consecutive clusters (637888 still unlabelled at
// ~40 minutes; 637937 still unlabelled when the e2e assert suite ran at 18:42).
//
// It is not a dead lane and not a permanent failure — it self-heals at the hour
// mark — which is precisely why it went unnoticed: every manual inspection either
// happened inside the hour (and looked broken) or after it (and looked fine).
//
// This is the same shape as the ESO store-recovery gap that `--reconcile-es-store-
// recovery` exists to close: a consumer level-triggered on the wrong signal, blind
// to its own precondition becoming satisfiable. The fix is symmetrical — watch for
// the precondition, and kick the lane when it flips.
//
// Why not just lower the resync floor: it would trade a bootstrap-only problem for
// a permanent 60x increase in Linode API polling on every cluster, to catch an
// event that happens exactly once in a cluster's life.

import (
	"context"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// linodeTokenPollInterval is how often the wrapper re-checks for the token during
// the bootstrap window. Cheap: a file/env read, no API call.
var linodeTokenPollInterval = 15 * time.Second

// linodeTokenPollBegan is called once this wrapper has SEEN the token absent and
// committed to polling for it. Production installs nothing; it exists so a test can
// order itself against a decision made on another goroutine.
//
// ────────────────────────────────────────────────────────────────────────────
// IT FIXES A FLAKE THAT LOOKED LIKE A SLOW MACHINE, and the shape is worth naming
// because a timing test that fails on its own deadline invites exactly the wrong
// fix (raise the deadline).
//
// waitForLinodeTokenThenKick early-returns when the token is ALREADY present —
// the steady state, where the wrapper must cost a live cluster nothing. Its test
// reproduces the bootstrap ordering by seeding the token part-way through, and
// nothing sequenced that write against the goroutine's own first check. Win the
// race and the poller is polling, sees the arrival, kicks. Lose it and the poller
// reads the token the test just wrote, concludes it is not needed, and returns
// WITHOUT KICKING — after which the test waits out its full 3s deadline and fails
// claiming the wrapper never noticed the arrival.
//
// Confirmed rather than guessed: delaying this function's first check by 50ms makes
// the test fail every time, with the same message and the same ~3s.
//
// RAISING THE DEADLINE WOULD HAVE MADE IT WORSE. The losing path never kicks at
// all, so a longer wait converts a fast flake into a slow one and the suite keeps
// passing on the strength of a race it wins most of the time.
// ────────────────────────────────────────────────────────────────────────────
var linodeTokenPollBegan = func() {}

// withLinodeTokenWait wraps a lane's watch so that, while the in-cluster Linode
// token is still absent, the lane is ALSO kicked on a short timer — and, crucially,
// once more at the moment the token first appears. After that the wrapper is inert
// and the lane is driven purely by its real watch.
//
// The kicks during the absent window are deliberate and near-free: each one runs a
// pass that requireLinodeToken turns into an immediate no-op. Their only job is to
// keep a scheduler slot alive so the transition can be noticed. The kick AFTER the
// transition is the one that does the work.
//
// The inner watch still owns the returned error and the lifetime: this only adds a
// goroutine that exits with ctx.
func withLinodeTokenWait(inner func(context.Context, func()) error) func(context.Context, func()) error {
	return func(ctx context.Context, onEvent func()) error {
		go waitForLinodeTokenThenKick(ctx, onEvent)
		return inner(ctx, onEvent)
	}
}

// waitForLinodeTokenThenKick polls until linode.InClusterLinodeToken() is non-empty, then
// fires onEvent once and returns. Returns immediately (without kicking) if the
// token is already present, which is the steady state on every pass after the first
// bootstrap — so this costs a live cluster nothing.
func waitForLinodeTokenThenKick(ctx context.Context, onEvent func()) {
	if linode.InClusterLinodeToken() != "" {
		return
	}
	t := time.NewTicker(linodeTokenPollInterval)
	defer t.Stop()
	// AFTER the absent-check and the ticker, so a waiter is guaranteed that a
	// token appearing from here on is seen by the loop below rather than by the
	// early return above.
	linodeTokenPollBegan()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if linode.InClusterLinodeToken() != "" {
				// The transition this whole file exists for: the lane's watch
				// events are long gone, so this kick is what actually relabels
				// and tags the Volumes created during bootstrap.
				onEvent()
				return
			}
		}
	}
}
