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
	now           func() time.Time

	mu       sync.RWMutex
	isLeader bool
}

func newLeaderElector(client leaseClient, namespace, name, identity string, now func() time.Time) *leaderElector {
	return &leaderElector{
		client:        client,
		namespace:     namespace,
		name:          name,
		identity:      identity,
		leaseDuration: 30 * time.Second,
		renewInterval: 10 * time.Second,
		now:           now,
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

func (e *leaderElector) setLeader(v bool) {
	e.mu.Lock()
	e.isLeader = v
	e.mu.Unlock()
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

// tryAcquire performs one acquire/renew evaluation.
func (e *leaderElector) tryAcquire(ctx context.Context) {
	obj, status, err := e.client.GetJSON(ctx, e.leasePath())
	if err != nil {
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
	case renew.IsZero() || e.now().Sub(renew) > e.leaseDuration:
		e.patchHeld(ctx, true) // holder's lease expired — take over
	default:
		e.setLeader(false) // held by a live peer
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
	e.setLeader(err == nil && status >= 200 && status < 300 && status != http.StatusConflict)
}

func (e *leaderElector) patchHeld(ctx context.Context, takeover bool) {
	if err := e.client.MergePatch(ctx, e.leasePath(), map[string]any{"spec": e.spec(takeover)}); err != nil {
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
