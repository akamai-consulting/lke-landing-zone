package baosnap

// restore.go — the restore path, ONCE, for both of its callers.
//
// `llz openbao restore` (the operator) and `llz ci assert-openbao-restore` (the
// gate) both go through Fetch and Apply. That is the whole reason this file is in
// a shared package: a drill that restored through its own copy of these steps
// would prove the copy works, and the operator's command could rot behind a green
// gate — the split-contract archetype, with the gate on the wrong side of it.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

// KeyAPI is the slice of the Linode client a restore needs: mint a short-lived
// key on the backup bucket, and revoke it. Taken as an interface so the caller
// supplies a client built under its OWN declared cloud grant.
type KeyAPI interface {
	CreateObjectStorageKeyBuckets(ctx context.Context, label, cluster string, buckets []string, permissions string) (map[string]any, error)
	DeleteObjectStorageKey(ctx context.Context, id uint64) error
}

// TempStore mints a key on bucket and returns a usable Store plus the revoke.
//
// WHY A FRESH KEY, NOT THE CRONJOB'S. The backup key lives in OpenBao — which is
// exactly what is gone on the day a restore is needed. So the operator path mints
// from LINODE_API_TOKEN (held in CI and by operators, never in OpenBao), scoped to
// the one bucket, `read_only` unless the caller needs to write, and deletes it on
// the way out. Its label is distinct from the rotator's so the rotator's
// keep-newest drain can never count or revoke it mid-restore.
func TempStore(ctx context.Context, api KeyAPI, label, objCluster, bucket, endpoint, permissions string) (Store, func(), error) {
	noop := func() {}
	if objCluster == "" || bucket == "" || endpoint == "" {
		return Store{}, noop, fmt.Errorf("cannot locate the backup bucket: obj cluster %q, bucket %q, endpoint %q", objCluster, bucket, endpoint)
	}
	m, err := api.CreateObjectStorageKeyBuckets(ctx, label, objCluster, []string{bucket}, permissions)
	if err != nil {
		return Store{}, noop, fmt.Errorf("mint a %s key on %s: %w", permissions, bucket, err)
	}
	id, ok := cli.AsUint64(m["id"])
	access, secret := cli.AsString(m["access_key"]), cli.AsString(m["secret_key"])
	revoke := func() {
		if !ok {
			return
		}
		if err := api.DeleteObjectStorageKey(context.Background(), id); err != nil {
			fmt.Printf("::warning::could not delete the temporary key %s (id=%d): %v — delete it in the Linode console\n", label, id, err)
		}
	}
	if !ok || access == "" || secret == "" {
		revoke()
		return Store{}, noop, fmt.Errorf("minting %s returned no id/access_key/secret_key", label)
	}
	MaskFn(secret)
	st := Store{AccessKey: access, SecretKey: secret, Endpoint: endpoint, Bucket: bucket}
	if err := st.WaitUsable(ctx, KeyPropagationBudget, KeyPollInterval); err != nil {
		revoke()
		return Store{}, noop, fmt.Errorf("the temporary key on %s never became usable: %w", bucket, err)
	}
	return st, revoke, nil
}

// KeyPropagationBudget bounds how long a freshly minted key may take to reach the
// object-storage gateways. Observed propagation is tens of seconds; two minutes
// tells "still propagating" from "scoped wrong" without stalling a restore.
var KeyPropagationBudget = 2 * time.Minute

// KeyPollInterval is how often TempStore re-tries a key that is still
// propagating.
var KeyPollInterval = 5 * time.Second

// MaskFn redacts a value from a CI log. Defaults to the GitHub Actions form, and
// is a no-op off Actions.
var MaskFn = func(v string) {
	if v != "" && isActions() {
		fmt.Printf("::add-mask::%s\n", v)
	}
}

// Fetch lists the bucket, picks the snapshot (see Select: key, or the newest —
// of fromCluster's, when set — when key is ""), downloads it, and VERIFIES it. A
// snapshot that does not verify is never handed to OpenBao: restoring a corrupt
// archive into a live store is the one outcome worse than not restoring.
func Fetch(ctx context.Context, st Store, key, fromCluster string) (Snapshot, []byte, Manifest, error) {
	objs, err := st.List(ctx, Prefix)
	if err != nil {
		return Snapshot{}, nil, Manifest{}, err
	}
	snaps, _ := Catalog(objs)
	snap, err := Select(snaps, key, fromCluster)
	if err != nil {
		return Snapshot{}, nil, Manifest{}, fmt.Errorf("%s: %w", st.Bucket, err)
	}
	data, err := st.Get(ctx, snap.Key)
	if err != nil {
		return snap, nil, Manifest{}, err
	}
	m, err := Verify(data)
	if err != nil {
		return snap, nil, Manifest{}, fmt.Errorf("%s does not verify — refusing to restore it: %w", snap.Key, err)
	}
	return snap, data, m, nil
}

// Target is a reachable OpenBao API — in practice a port-forward to the ACTIVE
// pod's loopback listener. Close tears the tunnel down.
type Target struct {
	Addr  string
	HTTP  *http.Client
	Close func()
}

// OpenActive resolves the pod currently carrying openbao-active=true and opens a
// tunnel to its LOOPBACK listener (no client certificate needed there; TLS is
// loopback-only, so it is not verified — see openbao.HTTPClientLoopback).
//
// run is the caller's cluster-read handle and portForward its tunnel opener: the
// caller supplies both because it holds the grants, not this package.
//
// Exactly one active pod, or an error. Zero means no leader (mid-election, or
// sealed); two means the labels are stale. Restoring into either guess is how a
// restore lands on a standby.
func OpenActive(run func(args ...string) ([]byte, error), portForward func(pod string) (string, func(), error)) (Target, error) {
	out, err := run("-n", baoread.Namespace, "get", "pods", "-l", openbao.ActivePodLabel,
		"-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return Target{}, fmt.Errorf("find the active OpenBao pod: %w", err)
	}
	pods := strings.Fields(string(out))
	if len(pods) != 1 {
		return Target{}, fmt.Errorf("expected exactly one pod labelled %s in %s, found %d (%v) — OpenBao has no single leader right now",
			openbao.ActivePodLabel, baoread.Namespace, len(pods), pods)
	}
	addr, stop, err := portForward(pods[0])
	if err != nil {
		return Target{}, fmt.Errorf("port-forward to %s: %w", pods[0], err)
	}
	return Target{Addr: addr, HTTP: openbao.HTTPClientLoopback(10 * time.Minute), Close: stop}, nil
}

// Plan is what a restore would do, computed before anything is changed.
type Plan struct {
	Snapshot Snapshot
	Manifest Manifest
	Live     Health
	// SameCluster: the snapshot's cluster id is the live cluster's. Only then is a
	// non-forced restore expected to succeed.
	SameCluster bool
}

// ErrNeedsForce is returned by Apply when the snapshot came from a different
// cluster and force was not given.
var ErrNeedsForce = errors.New("snapshot is from a different OpenBao cluster")

// Inspect reads the live cluster and builds the Plan. It changes nothing, which
// is what lets `llz openbao restore` default to a dry run that still says exactly
// which cluster it would overwrite.
func Inspect(ctx context.Context, t Target, snap Snapshot, m Manifest) (Plan, error) {
	h, err := ReadHealth(ctx, t.HTTP, t.Addr)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Snapshot: snap, Manifest: m, Live: h,
		SameCluster: h.ClusterID != "" && strings.EqualFold(h.ClusterID, snap.ClusterID)}, nil
}

// Apply restores data into the target. Guards, in order:
//
//   - the target must be initialized and unsealed — restore needs a working
//     barrier to land in, and a sealed node would refuse anyway with a less useful
//     message;
//   - the target must be the ACTIVE node (not a standby) — a restore through a
//     standby is not something to depend on forwarding for;
//   - a snapshot from a different cluster needs force. Different-cluster is the
//     HA peer (wrong kube-context) as often as it is a rebuilt cluster, and only
//     the operator can tell which.
//
// Then it waits, bounded, for the node to come back unsealed and serving: a
// restore that returned 204 to a node that never unsealed again is a failure,
// and saying "restored" there would be the vacuous green.
func Apply(ctx context.Context, t Target, token string, p Plan, data []byte, force bool, settle time.Duration) error {
	switch {
	case !p.Live.Initialized:
		return fmt.Errorf("the target OpenBao is not initialized — initialize it (with the SAME static seal key the snapshot's cluster used) before restoring into it")
	case p.Live.Sealed:
		return fmt.Errorf("the target OpenBao is sealed — check the openbao-unseal-key Secret holds the deployment's OPENBAO_SEAL_KEY")
	case p.Live.Standby:
		return fmt.Errorf("the target pod is a standby, not the active node — the restore must be sent to the pod labelled openbao-active=true")
	case !p.SameCluster && !force:
		return fmt.Errorf("%w: snapshot cluster %s, live cluster %s. If this deployment's OpenBao was REBUILT, re-run with --force. "+
			"If it was not, your kube-context is probably pointing at the OTHER cluster of an HA pair — stop", ErrNeedsForce,
			p.Snapshot.ClusterID, orUnknown(p.Live.ClusterID))
	}
	if err := Restore(ctx, t.HTTP, t.Addr, token, data, force); err != nil {
		if errors.Is(err, ErrForeignSnapshot) {
			return fmt.Errorf("%w — OpenBao itself says the snapshot's keyring is not this cluster's. "+
				"Only --force restores it, and only if this cluster was rebuilt with the SAME OPENBAO_SEAL_KEY", err)
		}
		return err
	}
	return WaitServing(ctx, t, settle)
}

// WaitServing polls health until the node is initialized, unsealed and active, or
// the budget runs out. An unreachable node is retried, not fatal: the active node
// may hand off leadership while it reloads the restored state.
func WaitServing(ctx context.Context, t Target, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var last string
	for {
		h, err := ReadHealth(ctx, t.HTTP, t.Addr)
		switch {
		case err != nil:
			last = err.Error()
		case h.Initialized && !h.Sealed:
			return nil
		default:
			last = fmt.Sprintf("initialized=%v sealed=%v standby=%v", h.Initialized, h.Sealed, h.Standby)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("OpenBao did not come back unsealed within %s after the restore (last: %s) — check the pods' logs and the openbao-unseal-key Secret", budget, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}
