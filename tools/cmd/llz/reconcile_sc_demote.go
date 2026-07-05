// The sc-demote watch reconciler (Phase 1) — the pure-Go, in-cluster port of the
// llz-cluster-foundation sc-default-patcher CronJob (see
// docs/designs/kube-native-reconciler.md).
//
// LKE's Flux-managed `workload` HelmRelease keeps re-marking the
// linode-block-storage-retain StorageClass as the cluster default. Two defaults
// hard-fail `llz ci converge`, and the Kyverno sc-default-demote policy alone
// can't fix it: it is admission-only (background: false, failurePolicy: Ignore),
// so once a `true` write slips past a briefly-unready webhook and Flux goes quiet
// (live == desired → no further admission events), the policy is STARVED and never
// re-demotes. The CronJob is the durable backstop that re-demotes on a fixed
// cadence regardless of events.
//
// This reconciler replaces that CronJob with the same durability guarantee: it
// WATCHES StorageClasses (fast, event-driven demote) AND carries a resync floor,
// so a slipped-through Flux re-promotion is re-demoted on the next resync tick
// even when no admission/watch event ever fires — exactly the starvation case the
// CronJob's */2 cadence covers. It DRIVES (patches the SC), so it is leader-gated.
package main

import (
	"context"
	"fmt"
)

const (
	scStorageClassesPath = "/apis/storage.k8s.io/v1/storageclasses"
	scDefaultAnnotation  = "storageclass.kubernetes.io/is-default-class"
	defaultDemoteSC      = "linode-block-storage-retain"
)

// reconcileSCDemote demotes the named StorageClass to non-default if it is
// currently marked the cluster default. Idempotent: a no-op when the SC is absent
// (single-class cluster) or already non-default. A patch failure surfaces (the
// manager records the pass failed).
func reconcileSCDemote(ctx context.Context, client reconcileClient, name string) error {
	obj, status, err := client.GetJSON(ctx, scStorageClassesPath+"/"+name)
	if err != nil {
		return err
	}
	if status == 404 || obj == nil {
		return nil // SC not present (LKE stopped defaulting its retain class, or single-class)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("GET storageclass %s: status %d", name, status)
	}
	if !scIsDefault(obj) {
		return nil // already non-default — nothing to do
	}
	return client.MergePatch(ctx, scStorageClassesPath+"/"+name, scDemotePatch())
}

// scIsDefault reports whether a StorageClass object carries
// is-default-class: "true". Defensive against missing/oddly-typed fields.
func scIsDefault(sc map[string]any) bool {
	meta, _ := sc["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	v, _ := ann[scDefaultAnnotation].(string)
	return v == "true"
}

// scDemotePatch rewrites is-default-class to "false" — the same mutation the
// Kyverno policy and the CronJob apply.
func scDemotePatch() map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{scDefaultAnnotation: "false"},
		},
	}
}
