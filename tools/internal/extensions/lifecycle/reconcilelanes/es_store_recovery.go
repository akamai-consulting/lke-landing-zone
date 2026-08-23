package reconcilelanes

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
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"
)

const (
	// esStorePath is the watched ClusterSecretStore (the read store every
	// platform ExternalSecret binds; openbao-push recovers with the same bump).
	esStorePath = "/apis/external-secrets.io/v1/clustersecretstores/" + DefaultSecretStore
	// ESStoresWatchPath is the collection watch scoped to the read store by
	// field selector (RBAC list/watch cannot be resourceNames-scoped).
	ESStoresWatchPath = "/apis/external-secrets.io/v1/clustersecretstores?fieldSelector=metadata.name%3D" + DefaultSecretStore
	// esListPath / pushListPath are the cluster-wide collections the bump
	// fans out over (PushSecret is still v1alpha1 upstream).
	esListPath   = "/apis/external-secrets.io/v1/externalsecrets"
	pushListPath = "/apis/external-secrets.io/v1alpha1/pushsecrets"
)

// ESStoreRecovery carries the lane's poll-to-poll memory: the store's last
// observed readiness ("" until first observed, else "true"/"false") and how many
// consecutive fan-outs have failed while holding the transition open.
type ESStoreRecovery struct {
	lastReady string
	fanoutErr int
}

// esFanoutRetryBudget bounds how long an unconsumed transition may be retried.
//
// WITHHOLDING THE TRANSITION IS ONLY SAFE IF THE FAILURE CAN STOP. Retrying
// forever turns a STABLY failing condition into a permanent write loop: this lane
// runs on a 300s resync AND on every ClusterSecretStore watch event, and each
// retry MergePatches a fresh force-sync stamp onto every ExternalSecret in the
// cluster, each of which makes ESO do a full fetch from OpenBao. Meanwhile
// llz_reconcile_up sits at 0 and llz_reconcile_errors_total climbs, so
// LLZReconcilerReportingDown and LLZReconcilerErroring page continuously about a
// loop that will never converge on its own.
//
// After the budget the transition is CONSUMED and the error still returned, so
// the failure stays visible as an error rather than as an outage. A real
// transient — a 409, a rolling apiserver — is gone long before three passes.
const esFanoutRetryBudget = 3

// reconcileESStoreRecovery reads the store's Ready condition, publishes the
// llz_es_store_ready gauge, and bumps every ExternalSecret/PushSecret when the
// store transitions to Ready (or is Ready on the first pass with a bound
// ExternalSecret still not-Ready — the restart-amnesty case).
// Reconcile takes the FENCED handle for the same reason SCDemote does: it Gets and
// Patches ExternalSecrets and never watches, so its parameter is exactly what its
// binding declared.
func (s *ESStoreRecovery) Reconcile(ctx context.Context, client capability.KubeAPI, reg *metrics.Registry) error {
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
	ready := ObjReadyStatus(obj) == "True"
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
	if !bump {
		s.lastReady = fmt.Sprintf("%t", ready)
		return nil
	}

	// THE TRANSITION IS CONSUMED ONLY BY A FAN-OUT THAT SUCCEEDED, and recording it
	// first is what made this lane lose work permanently. lastReady was set before
	// forceSyncESKinds, so a failed or PARTIAL fan-out — one MergePatch 409, one
	// list 403, a CRD version drift on pushsecrets — still left lastReady "true".
	// The next poll then reads neither `ready && lastReady == "false"` nor the
	// first-observation amnesty branch, so it never bumps again: the
	// notReady->Ready transition this lane exists to catch is spent, and every
	// ExternalSecret the fan-out missed is left to ESO's own ~16-minute backoff
	// with nothing retrying it.
	//
	// The restart-amnesty branch above already gets this ordering right (it only
	// bumps when something is observably stale), which is what makes the
	// difference legible: one path re-derives its trigger from the world, the other
	// held its trigger in memory and threw it away before doing the work.
	bumped, err := forceSyncESKinds(ctx, client)
	if err != nil {
		s.fanoutErr++
		if s.fanoutErr < esFanoutRetryBudget {
			// lastReady deliberately UNCHANGED: the next poll re-enters this branch
			// and re-runs the fan-out. force-sync is an annotation write with a
			// fresh timestamp, so repeating it is free — for a BOUNDED number of
			// passes. See esFanoutRetryBudget for why it cannot be unbounded.
			fmt.Printf("es-store-recovery: store Ready — force-sync incomplete after %d object(s) (%v); "+
				"leaving the transition unconsumed, retry %d/%d\n", bumped, err, s.fanoutErr, esFanoutRetryBudget)
			return err
		}
		s.lastReady = fmt.Sprintf("%t", ready)
		s.fanoutErr = 0
		fmt.Printf("es-store-recovery: store Ready — force-sync has failed %d consecutive times (%v); "+
			"CONSUMING the transition so this lane stops re-patching every ExternalSecret in the cluster "+
			"every pass. %d object(s) were bumped. This is a stable failure, not a slow one — fix the "+
			"underlying error; ESO's own ~16m backoff is what retries the rest.\n",
			esFanoutRetryBudget, err, bumped)
		return err
	}
	s.lastReady = fmt.Sprintf("%t", ready)
	s.fanoutErr = 0
	// COUNTED ON WORK DONE, not on the absence of an error. `err == nil` made a
	// fan-out that patched nothing indistinguishable from one that patched
	// everything on the one counter an operator would consult to ask which it was —
	// and this counter is the recorded evidence that authorised deleting the
	// CI-side ES force-sync (docs/designs/secrets-before-apps.md).
	if bumped > 0 {
		reg.AddCounter("llz_es_recovery_nudges_total",
			"count of store-recovery force-sync fan-outs (one per Ready transition)", nil, 1)
	}
	fmt.Printf("es-store-recovery: store Ready — force-synced %d ExternalSecret/PushSecret object(s)\n", bumped)
	return nil
}

// ObjReadyStatus is EXPORTED for one reason: package main's
// TestReadyConditionAgreesWithFindReady asserts that this reader and the runtime's
// readyCondition never disagree about a Ready condition, and the extraction put the
// two on opposite sides of a package boundary. A coupling test that cannot reach
// one of the two things it couples is not a test.
//
// ObjReadyStatus extracts a resource's Ready condition status via the shared
// health predicate ("" when absent).
func ObjReadyStatus(obj map[string]any) string {
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
func anyExternalSecretNotReady(ctx context.Context, client capability.KubeAPI) (bool, error) {
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
		if ObjReadyStatus(m) != "True" {
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
func forceSyncESKinds(ctx context.Context, client capability.KubeAPI) (int, error) {
	patch := map[string]any{"metadata": map[string]any{
		"annotations": map[string]any{"force-sync": fmt.Sprintf("%d", nowUnix())},
	}}
	bumped := 0
	var firstErr error
	for _, listPath := range []string{esListPath, pushListPath} {
		obj, status, err := client.GetJSON(ctx, listPath)
		if err == nil && status == 404 {
			// THE KIND IS NOT SERVED — not applicable, not a failure. PushSecret is
			// still v1alpha1 upstream and a cluster on a different CRD version simply
			// has no such collection. Counting that as an error kept firstErr
			// permanently non-nil, and the caller withholds its transition on any
			// error: a cluster without PushSecrets would have re-patched every
			// ExternalSecret it has, on every 300s resync and every watch event,
			// forever. Skip it and let ExternalSecrets get their bump.
			continue
		}
		if err != nil || status < 200 || status >= 300 || obj == nil {
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
