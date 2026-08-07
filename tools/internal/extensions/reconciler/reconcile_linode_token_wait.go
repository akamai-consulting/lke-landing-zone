package reconciler

// reconcile_linode_token_wait.go closes the first-boot gap between a
// linode-dependent reconciler lane's TRIGGERS and its PRECONDITION.
//
// THE BUG (measured on lke637937, release-e2e run 30568560562):
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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/credrotate"
)

// linodeTokenPollInterval is how often the wrapper re-checks for the token during
// the bootstrap window. Cheap: a file/env read, no API call.
var linodeTokenPollInterval = 15 * time.Second

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

// waitForLinodeTokenThenKick polls until credrotate.InClusterLinodeToken() is non-empty, then
// fires onEvent once and returns. Returns immediately (without kicking) if the
// token is already present, which is the steady state on every pass after the first
// bootstrap — so this costs a live cluster nothing.
func waitForLinodeTokenThenKick(ctx context.Context, onEvent func()) {
	if credrotate.InClusterLinodeToken() != "" {
		return
	}
	t := time.NewTicker(linodeTokenPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if credrotate.InClusterLinodeToken() != "" {
				// The transition this whole file exists for: the lane's watch
				// events are long gone, so this kick is what actually relabels
				// and tags the Volumes created during bootstrap.
				onEvent()
				return
			}
		}
	}
}
