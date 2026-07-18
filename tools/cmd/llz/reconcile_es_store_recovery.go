package main

// The ES store-recovery watch reconciler (secrets-before-apps Phase 2 — see
// docs/designs/secrets-before-apps.md). ESO's ExternalSecret controller does
// NOT watch SecretStore status: when the `openbao` ClusterSecretStore recovers
// (post unseal+seed at bootstrap, or after a day-2 blip), every bound
// ExternalSecret/PushSecret idles on its own error backoff (~16m ceiling for a
// never-synced object) or refreshInterval. This lane watches the store's Ready
// condition and, on a not-Ready→Ready transition, bumps a `force-sync`
// annotation on every ExternalSecret AND PushSecret — collapsing the recovery
// gap to seconds, in-cluster, event-driven. It supersedes the CI-imperative
// half of `llz ci nudge-argo` (which also never covered PushSecrets).
//
// Same driver-conversion justification as the argo-nudge lane: the force-sync
// bump is an already-documented driver moving from CI-imperative to
// watch-triggered (convergence-contract anti-patterns #4/#6).
//
// Transition tracking is in-memory and deliberately restart-amnesiac: on the
// first pass after a (re)start, a Ready store with any bound ExternalSecret
// still not-Ready gets one bump — idempotent (a redundant bump costs one cheap
// ESO reconcile), so losing state can only cost an extra no-op, never a missed
// recovery.

import (
	"context"
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
)

const (
	// esStorePath is the watched ClusterSecretStore (the read store every
	// platform ExternalSecret binds; openbao-push recovers with the same bump).
	esStorePath = "/apis/external-secrets.io/v1/clustersecretstores/" + defaultSecretStore
	// esStoresWatchPath is the collection watch scoped to the read store by
	// field selector (RBAC list/watch cannot be resourceNames-scoped).
	esStoresWatchPath = "/apis/external-secrets.io/v1/clustersecretstores?fieldSelector=metadata.name%3D" + defaultSecretStore
	// esListPath / pushListPath are the cluster-wide collections the bump
	// fans out over (PushSecret is still v1alpha1 upstream).
	esListPath   = "/apis/external-secrets.io/v1/externalsecrets"
	pushListPath = "/apis/external-secrets.io/v1alpha1/pushsecrets"
)

// esStoreRecovery carries the lane's poll-to-poll memory: the store's last
// observed readiness ("" until first observed, else "true"/"false").
type esStoreRecovery struct {
	lastReady string
}

// reconcileESStoreRecovery reads the store's Ready condition, publishes the
// llz_es_store_ready gauge, and bumps every ExternalSecret/PushSecret when the
// store transitions to Ready (or is Ready on the first pass with a bound
// ExternalSecret still not-Ready — the restart-amnesty case).
func (s *esStoreRecovery) reconcile(ctx context.Context, client reconcileClient, reg *metrics.Registry) error {
	obj, status, err := client.GetJSON(ctx, esStorePath)
	if err != nil {
		return err
	}
	if status == 404 {
		// Store not created yet (pre-bootstrap) — observed, not an error.
		reg.SetGauge("llz_es_store_ready", "1 if the openbao ClusterSecretStore reports Ready", nil, 0)
		s.lastReady = "false"
		return nil
	}
	if status < 200 || status >= 300 || obj == nil {
		return fmt.Errorf("GET %s: status %d", esStorePath, status)
	}
	ready := objReadyStatus(obj) == "True"
	v := 0.0
	if ready {
		v = 1
	}
	reg.SetGauge("llz_es_store_ready", "1 if the openbao ClusterSecretStore reports Ready", nil, v)

	bump := false
	switch {
	case ready && s.lastReady == "false":
		bump = true // the recovery transition proper
	case ready && s.lastReady == "":
		// First observation after a (re)start: if any ExternalSecret is still
		// not-Ready the recovery may have happened while no leader watched — bump
		// once. All-Ready means nothing to recover.
		stale, err := anyExternalSecretNotReady(ctx, client)
		if err != nil {
			return err
		}
		bump = stale
	}
	s.lastReady = fmt.Sprintf("%t", ready)
	if !bump {
		return nil
	}

	bumped, err := forceSyncESKinds(ctx, client)
	reg.AddCounter("llz_es_recovery_nudges_total",
		"count of store-recovery force-sync fan-outs (one per Ready transition)", nil, 1)
	fmt.Printf("es-store-recovery: store Ready — force-synced %d ExternalSecret/PushSecret object(s)\n", bumped)
	return err
}

// objReadyStatus extracts a resource's Ready condition status via the shared
// health predicate ("" when absent).
func objReadyStatus(obj map[string]any) string {
	statusObj, _ := obj["status"].(map[string]any)
	rawConds, _ := statusObj["conditions"].([]any)
	conds := make([]health.Condition, 0, len(rawConds))
	for _, rc := range rawConds {
		m, ok := rc.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		st, _ := m["status"].(string)
		conds = append(conds, health.Condition{Type: typ, Status: st})
	}
	st, _, _ := health.FindReady(conds)
	return st
}

// anyExternalSecretNotReady reports whether any ExternalSecret in the cluster
// is not (yet) Ready — the restart-amnesty probe.
func anyExternalSecretNotReady(ctx context.Context, client reconcileClient) (bool, error) {
	obj, status, err := client.GetJSON(ctx, esListPath)
	if err != nil {
		return false, err
	}
	if status < 200 || status >= 300 || obj == nil {
		return false, fmt.Errorf("GET %s: status %d", esListPath, status)
	}
	items, _ := obj["items"].([]any)
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if objReadyStatus(m) != "True" {
			return true, nil
		}
	}
	return false, nil
}

// forceSyncESKinds bumps the force-sync annotation on every ExternalSecret and
// PushSecret cluster-wide (a CHANGED annotation value is what triggers an
// immediate ESO reconcile). One object's patch failure does not stop the
// fan-out; the first error is returned so the manager records the pass failed
// and the resync floor retries.
func forceSyncESKinds(ctx context.Context, client reconcileClient) (int, error) {
	patch := map[string]any{"metadata": map[string]any{
		"annotations": map[string]any{"force-sync": fmt.Sprintf("%d", nowUnix())},
	}}
	bumped := 0
	var firstErr error
	for _, listPath := range []string{esListPath, pushListPath} {
		obj, status, err := client.GetJSON(ctx, listPath)
		if err != nil || status < 200 || status >= 300 || obj == nil {
			// PushSecrets may legitimately be absent (CRD version drift) — record
			// and continue so ExternalSecrets still get their bump.
			if firstErr == nil {
				if err == nil {
					err = fmt.Errorf("status %d", status)
				}
				firstErr = fmt.Errorf("GET %s: %w", listPath, err)
			}
			continue
		}
		items, _ := obj["items"].([]any)
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			meta, _ := m["metadata"].(map[string]any)
			name, _ := meta["name"].(string)
			ns, _ := meta["namespace"].(string)
			if name == "" || ns == "" {
				continue
			}
			if err := client.MergePatch(ctx, esItemPath(listPath, ns, name), patch); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("force-sync %s/%s: %w", ns, name, err)
				}
				continue
			}
			bumped++
		}
	}
	return bumped, firstErr
}

// esItemPath converts a cluster-wide collection path into the namespaced item
// path for one object (…/v1/externalsecrets → …/v1/namespaces/<ns>/externalsecrets/<name>).
func esItemPath(listPath, ns, name string) string {
	group := listPath[:len(listPath)-len("/externalsecrets")]
	kind := "externalsecrets"
	if listPath == pushListPath {
		group = listPath[:len(listPath)-len("/pushsecrets")]
		kind = "pushsecrets"
	}
	return group + "/namespaces/" + ns + "/" + kind + "/" + name
}
