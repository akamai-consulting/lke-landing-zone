// The argo-resync-nudger watch reconciler (Phase 1) — the pure-Go, in-cluster
// port of the argo-resync-nudger CronJob. Argo CD does NOT auto-retry a sync that
// terminally failed to a revision (selfHeal only corrects drift AFTER a
// successful sync), so a first-boot ordering race that exhausts an Application's
// retry budget leaves it wedged until a human syncs it. This reconciler watches
// Applications and re-triggers exactly those whose last sync operation phase is
// "Failed" — reacting in seconds instead of the CronJob's up-to-3-minute poll.
//
// It DRIVES (patches Applications), same as the CronJob does today — converting
// poll→watch is not a new driver (convergence-contract anti-pattern #4 is about
// adding a side-controller to drive what a reconciler should observe; this is the
// same documented driver, event-triggered). Scope is unchanged: only phase=Failed
// Applications, which Argo genuinely will not self-heal. Idempotent: patching a
// sync operation moves the app out of Failed, so the next pass no-ops it.
package main

import (
	"context"
	"fmt"
)

// argoAppsPath is the Applications collection (Argo CD installs them in argocd).
const argoAppsPath = "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications"

// argoClient is the slice of the kube client the nudger needs.
type argoClient interface {
	GetJSON(ctx context.Context, path string) (map[string]any, int, error)
	MergePatch(ctx context.Context, path string, patch any) error
}

// reconcileArgoNudge lists Applications and re-triggers each terminally-failed
// one. A patch failure on one app does not abort the pass — the rest are still
// nudged; the first error is returned so the manager records the pass as failed.
func reconcileArgoNudge(ctx context.Context, client argoClient) error {
	obj, status, err := client.GetJSON(ctx, argoAppsPath)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 || obj == nil {
		return fmt.Errorf("GET applications: status %d", status)
	}
	items, _ := obj["items"].([]any)
	var firstErr error
	for _, it := range items {
		name, failed := failedAppName(it)
		if !failed {
			continue
		}
		if err := client.MergePatch(ctx, argoAppsPath+"/"+name, argoNudgePatch()); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("nudge %s: %w", name, err)
			}
		}
	}
	return firstErr
}

// failedAppName returns an Application's name and whether its last sync operation
// terminally failed (status.operationState.phase == "Failed"). Defensive against
// missing/oddly-typed fields — anything malformed is "not failed", never a panic.
func failedAppName(item any) (string, bool) {
	m, ok := item.(map[string]any)
	if !ok {
		return "", false
	}
	meta, _ := m["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	status, _ := m["status"].(map[string]any)
	opState, _ := status["operationState"].(map[string]any)
	phase, _ := opState["phase"].(string)
	return name, name != "" && phase == "Failed"
}

// argoNudgePatch is the merge patch that re-triggers an Application: a hard
// refresh annotation plus a fresh sync operation — the same two actions the
// CronJob's `kubectl annotate` + `kubectl patch` did, folded into one patch.
func argoNudgePatch() map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{"argocd.argoproj.io/refresh": "hard"},
		},
		"operation": map[string]any{
			"initiatedBy": map[string]any{"username": "llz-reconciler"},
			"sync":        map[string]any{},
		},
	}
}
