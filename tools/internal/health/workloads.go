package health

import (
	"fmt"
	"time"
)

// workloads.go ports section 3 (Deployments / StatefulSets / DaemonSets), 3a
// (PVC binding), and 1f (controller Lease freshness) — the replica-count,
// rollout, and time-based predicates, all reusing the deferred/phase-1 matchers.

// ExpectedLinodeBlockStoragePVCs are the named PVCs whose charts hardcode the
// LKE-default linode-block-storage class (Kyverno mutation can't reach them on a
// cold bootstrap) — an expected deviation, not a failure.
func ExpectedLinodeBlockStoragePVCs() []DepEntry {
	return []DepEntry{
		{"gitea/valkey-data-gitea-valkey-primary-0", "gitea-valkey hardcodes linode-block-storage; Kyverno mutation lagged the chart-install. Re-roll (STS scale-down + PVC delete + scale-up), or live with the unencrypted ephemeral cache."},
		{"istio-system/data-oauth2-proxy-redis-ha-server-0", "oauth2-proxy redis-HA hardcodes linode-block-storage; same remediation as gitea-valkey."},
	}
}

// ClassifyWorkload classifies a Deployment or StatefulSet by ready/desired
// replicas. A scaled-down (desired 0) or fully-ready workload is CatOK; a
// not-ready one routes through the Phase-1 cascade and operator-deferred
// allowlists before failing. progressingReason/Msg annotate a genuine failure
// (Deployments carry a Progressing condition; pass "" for StatefulSets).
func ClassifyWorkload(kind, ns, name string, desired, ready int, progressingReason, progressingMsg string, phase1 bool) (Category, string) {
	label := fmt.Sprintf("%s %s/%s (%d/%d)", kind, ns, name, ready, desired)
	if desired == 0 || ready == desired {
		return CatOK, label
	}
	key := ns + "/" + name
	if phase1 && MatchPrefix(key, Phase1PendingWorkloads()) {
		return CatPending, label + " — waiting on OpenBao bootstrap"
	}
	if reason, ok := MatchExternalDep(key, ExternalDepWorkloads()); ok {
		return CatDeferred, label + " — " + reason
	}
	extra := ""
	if progressingReason != "" {
		extra = " — Progressing=" + progressingReason
		if progressingReason == "ProgressDeadlineExceeded" && progressingMsg != "" {
			extra += ": " + progressingMsg
		}
	}
	return CatFail, label + extra
}

// ClassifyDaemonSet checks a DaemonSet beyond ready-count: a stalled rolling
// update (updated < desired) and misscheduled pods are silent failures the
// ready-count alone misses.
func ClassifyDaemonSet(ns, name string, desired, ready, updated, missched int) (Category, string) {
	base := fmt.Sprintf("DaemonSet %s/%s", ns, name)
	switch {
	case ready != desired:
		return CatFail, fmt.Sprintf("%s (%d/%d ready)", base, ready, desired)
	case updated != desired:
		return CatFail, fmt.Sprintf("%s rolling update stalled (updated=%d desired=%d) — check PDB / pod schedulability", base, updated, desired)
	case missched != 0:
		return CatFail, fmt.Sprintf("%s has %d misscheduled pod(s) — selector drift or stale tolerations", base, missched)
	default:
		return CatOK, fmt.Sprintf("%s (%d/%d ready, %d updated, 0 misscheduled)", base, ready, desired, updated)
	}
}

// ClassifyPVC classifies a PersistentVolumeClaim: any non-Bound phase fails;
// Bound on the LKE-default linode class is an expected deviation (deferred) for
// the named chart-hardcoded PVCs, otherwise a warn; Bound on any other class is OK.
func ClassifyPVC(ns, name, status, class string) (Category, string) {
	if status != "Bound" {
		return CatFail, fmt.Sprintf("PVC %s/%s (%s) status=%s", ns, name, class, status)
	}
	if class == "linode-block-storage" || class == "linode-block-storage-retain" {
		if reason, ok := MatchExternalDep(ns+"/"+name, ExpectedLinodeBlockStoragePVCs()); ok {
			return CatDeferred, fmt.Sprintf("PVC %s/%s Bound on %s (expected) — %s", ns, name, class, reason)
		}
		return CatWarn, fmt.Sprintf("PVC %s/%s Bound on %s — not block-storage-retain; Kyverno mutation may have lagged the chart-install; re-roll to encrypted+tagged storage.", ns, name, class)
	}
	return CatOK, fmt.Sprintf("PVC %s/%s (%s) Bound", ns, name, class)
}

// LeaseStale reports whether a controller Lease is stale — its renewTime older
// than 4× leaseDuration, meaning no active leader (the controller silently
// stopped reconciling while its Deployment still looks Healthy). A non-positive
// duration falls back to the k8s default of 15s (jq `// 15`).
func LeaseStale(renew, now time.Time, durationSeconds int) bool {
	if durationSeconds <= 0 {
		durationSeconds = 15
	}
	return now.Sub(renew).Seconds() > float64(durationSeconds*4)
}
