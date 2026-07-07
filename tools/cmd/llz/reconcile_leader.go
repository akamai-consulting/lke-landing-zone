// Lease-based leader election for `llz reconcile` (see
// docs/designs/kube-native-reconciler.md).
//
// The observe reconciler is read-only and runs on every replica. The DRIVING
// reconcilers (argo-nudge, linode-creds, harbor) must have a single writer, so
// they run only on the pod that holds the lease — otherwise a rollout window or a
// scaled-up Deployment could double-mint Linode keys or double-patch Applications.
//
// This is a minimal coordination.k8s.io/v1 Lease elector over the hand-rolled
// kube client (no client-go leaderelection): acquire (create if absent, take over
// if the holder's lease has expired), renew while held, step down if a peer takes
// it. It is intentionally simple — a few-second lag on failover is fine because
// the driving reconcilers are idempotent and level-based, so a brief gap or a
// one-off double-run on takeover is harmless.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// leaseClient is the slice of the kube client the elector needs.
type leaseClient interface {
	GetJSON(ctx context.Context, path string) (map[string]any, int, error)
	CreateJSON(ctx context.Context, path string, obj any) (int, error)
	MergePatch(ctx context.Context, path string, patch any) error
}

type leaderElector struct {
	client        leaseClient
	namespace     string
	name          string
	identity      string
	leaseDuration time.Duration
	renewInterval time.Duration
	// opTimeout bounds a single acquire/renew evaluation's API calls with a
	// context deadline, INDEPENDENT of the kube client's own (longer) timeout, so a
	// slow or stuck call cannot stall the renew loop for seconds and a hung
	// connection is cancelled at the transport layer rather than waited out.
	opTimeout time.Duration
	// staleAfter is how long the loop may go without completing an evaluation
	// before Healthy() reports it wedged (a liveness signal): a live loop stamps
	// lastEval every renewInterval even when it errors or stands back, so only a
	// genuinely stuck goroutine trips it — never a legitimate non-leader standby.
	staleAfter time.Duration
	now        func() time.Time

	mu       sync.RWMutex
	isLeader bool
	lastEval time.Time // set to now() on construction and after every tryAcquire

	// error-log rate limiting; touched only from the single run() goroutine.
	lastErrMsg string
	lastErrAt  time.Time
}

func newLeaderElector(client leaseClient, namespace, name, identity string, now func() time.Time) *leaderElector {
	return &leaderElector{
		client:        client,
		namespace:     namespace,
		name:          name,
		identity:      identity,
		leaseDuration: 30 * time.Second,
		renewInterval: 10 * time.Second,
		opTimeout:     5 * time.Second,
		staleAfter:    30 * time.Second,
		now:           now,
		lastEval:      now(),
	}
}

func (e *leaderElector) leasePath() string {
	return fmt.Sprintf("/apis/coordination.k8s.io/v1/namespaces/%s/leases/%s", e.namespace, e.name)
}

func (e *leaderElector) collectionPath() string {
	return fmt.Sprintf("/apis/coordination.k8s.io/v1/namespaces/%s/leases", e.namespace)
}

// IsLeader reports whether this pod currently holds the lease.
func (e *leaderElector) IsLeader() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.isLeader
}

// setLeader records leadership and logs the transition (only on change, so a
// steady leader/standby is silent). The elector otherwise logs nothing on the
// happy path — these lines plus logErr are what make a wedge diagnosable.
func (e *leaderElector) setLeader(v bool) {
	e.mu.Lock()
	changed := e.isLeader != v
	e.isLeader = v
	e.mu.Unlock()
	if !changed {
		return
	}
	if v {
		log.Printf("leader-election: ACQUIRED lease %s/%s as %s", e.namespace, e.name, e.identity)
	} else {
		log.Printf("leader-election: lost leadership of lease %s/%s (%s)", e.namespace, e.name, e.identity)
	}
}

// markEval stamps the completion of one evaluation so Healthy() can tell a live
// loop (even one that keeps erroring) from a wedged goroutine.
func (e *leaderElector) markEval() {
	e.mu.Lock()
	e.lastEval = e.now()
	e.mu.Unlock()
}

// Healthy reports whether the elector loop is making progress — it completed a
// tryAcquire evaluation within staleAfter. A wedged goroutine (a hung call the
// opTimeout somehow doesn't cover, a deadlock, or one that never started) stops
// stamping and trips this, so the liveness probe restarts the pod instead of it
// sitting silently leaderless forever. A healthy STANDBY still evaluates every
// renewInterval, so this never flags a legitimate non-leader.
func (e *leaderElector) Healthy() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.now().Sub(e.lastEval) <= e.staleAfter
}

// logErr surfaces an acquire/renew error, rate-limited so a persistent failure
// (e.g. the API path unreachable) logs on change and then at most once per
// staleAfter instead of every renewInterval — loud enough to diagnose, not a
// flood. Called only from the run() goroutine.
func (e *leaderElector) logErr(op string, err error) {
	msg := fmt.Sprintf("%s: %v", op, err)
	now := e.now()
	if msg == e.lastErrMsg && now.Sub(e.lastErrAt) < e.staleAfter {
		return
	}
	e.lastErrMsg, e.lastErrAt = msg, now
	log.Printf("leader-election: %s", msg)
}

// run acquires/renews on renewInterval until ctx is cancelled, then best-effort
// releases so a peer can take over without waiting out the full lease duration.
func (e *leaderElector) run(ctx context.Context) {
	e.tryAcquire(ctx)
	t := time.NewTicker(e.renewInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			e.release()
			return
		case <-t.C:
			e.tryAcquire(ctx)
		}
	}
}

// tryAcquire performs one acquire/renew evaluation. Its API calls are bounded by
// opTimeout so a slow/stuck call can't stall the loop, and it stamps lastEval on
// every return so a live-but-erroring loop stays Healthy while a wedged one does
// not.
func (e *leaderElector) tryAcquire(ctx context.Context) {
	defer e.markEval()
	ctx, cancel := context.WithTimeout(ctx, e.opTimeout)
	defer cancel()

	obj, status, err := e.client.GetJSON(ctx, e.leasePath())
	if err != nil {
		e.logErr("lease read failed", err)
		e.setLeader(false) // can't confirm the lease — do not drive
		return
	}
	if status == http.StatusNotFound || obj == nil {
		e.create(ctx)
		return
	}
	holder, renew := leaseHolderRenew(obj)
	switch {
	case holder == e.identity:
		e.patchHeld(ctx, false) // still ours — renew
	case holder == "" || renew.IsZero() || e.now().Sub(renew) > e.leaseDuration:
		// Released (a peer's graceful step-down clears holderIdentity but leaves a
		// fresh renewTime), never-held, or expired — all takeable NOW. Without the
		// holder=="" case a released lease falls to the default branch and is
		// mistaken for a live peer, so a successor waits out the full leaseDuration
		// (up to 30s of no leader) after every rollout even though release() cleared
		// the holder precisely to hand off immediately.
		e.patchHeld(ctx, true)
	default:
		e.setLeader(false) // held by a live peer still renewing
	}
}

func (e *leaderElector) create(ctx context.Context) {
	lease := map[string]any{
		"apiVersion": "coordination.k8s.io/v1",
		"kind":       "Lease",
		"metadata":   map[string]any{"name": e.name, "namespace": e.namespace},
		"spec":       e.spec(true),
	}
	status, err := e.client.CreateJSON(ctx, e.collectionPath(), lease)
	// 409 → a peer created it first; not leader this round (next tick re-evaluates).
	switch {
	case err != nil:
		e.logErr("lease create failed", err)
	case status >= 300 && status != http.StatusConflict:
		e.logErr("lease create rejected", fmt.Errorf("HTTP %d", status))
	}
	e.setLeader(err == nil && status >= 200 && status < 300 && status != http.StatusConflict)
}

func (e *leaderElector) patchHeld(ctx context.Context, takeover bool) {
	if err := e.client.MergePatch(ctx, e.leasePath(), map[string]any{"spec": e.spec(takeover)}); err != nil {
		if takeover {
			e.logErr("lease takeover failed", err)
		} else {
			e.logErr("lease renew failed", err)
		}
		e.setLeader(false)
		return
	}
	e.setLeader(true)
}

// spec builds a Lease spec claiming the lease for this identity as of now.
// acquireTime is set only when taking the lease fresh (create/takeover).
func (e *leaderElector) spec(setAcquire bool) map[string]any {
	now := e.now().UTC().Format(time.RFC3339Nano)
	s := map[string]any{
		"holderIdentity":       e.identity,
		"leaseDurationSeconds": int(e.leaseDuration.Seconds()),
		"renewTime":            now,
	}
	if setAcquire {
		s["acquireTime"] = now
	}
	return s
}

// release clears holderIdentity if we hold it, so a peer can take over promptly.
// Best-effort with its own short context (the run ctx is already cancelled).
func (e *leaderElector) release() {
	if !e.IsLeader() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = e.client.MergePatch(ctx, e.leasePath(), map[string]any{"spec": map[string]any{"holderIdentity": nil}})
	e.setLeader(false)
}

// leaseHolderRenew extracts holderIdentity and renewTime from a Lease object,
// defensive against missing/oddly-typed fields.
func leaseHolderRenew(obj map[string]any) (string, time.Time) {
	spec, _ := obj["spec"].(map[string]any)
	holder, _ := spec["holderIdentity"].(string)
	rt, _ := spec["renewTime"].(string)
	t, _ := time.Parse(time.RFC3339, rt) // RFC3339 parses the RFC3339Nano we write
	return holder, t
}
