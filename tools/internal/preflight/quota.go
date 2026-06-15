// Package preflight holds the pure capacity-guard arithmetic ported out of
// preflight-quota.sh: given current account usage + an apply's additions + an
// operator-supplied limit, decide whether the apply would exceed quota. The
// orphan-identity heuristics it pairs with live in internal/linode (the same
// ones `llz reap` / `llz ci reap-*` use); this package is just the math, so the
// off-by-one-prone "+1 VPC" / "> limit" boundaries are unit-tested.
package preflight

// A limit of 0 (or negative) means "unset" — no public Linode quota API exists,
// so limits are operator-supplied and an unset limit is report-only (never fails).

// VPCQuotaExceeded reports whether creating `adds` VPCs on top of `total`
// existing would exceed `limit`. An unset limit never trips.
func VPCQuotaExceeded(total, adds, limit int) bool {
	return limit > 0 && total+adds > limit
}

// VCPUQuotaExceeded reports whether a pool of `pool` vCPUs on top of `used`
// would exceed `limit`. An unset limit never trips.
func VCPUQuotaExceeded(used, pool, limit int) bool {
	return limit > 0 && used+pool > limit
}

// PoolVCPU is the vCPU draw of a node pool: per-node vCPUs × node count.
func PoolVCPU(typeVCPU, nodeCount int) int {
	return typeVCPU * nodeCount
}

// SameLabelExcess reports whether more than one live LKE cluster already carries
// the label this apply is about to create — a healthy account has at most one
// (the managed cluster on a re-apply); the rest are orphans from failed runs.
func SameLabelExcess(liveWithLabel int) bool {
	return liveWithLabel > 1
}

// OrphansExceedThreshold reports whether the orphan count is over the threshold
// (the preflight fails/warns only when it is).
func OrphansExceedThreshold(orphans, threshold int) bool {
	return orphans > threshold
}
