package kube

// lease.go — Lease wire-format parsing.
//
// IT LIVES HERE, and it started as a Deps field on internal/assertreconciler.
// That was the wrong seam and the tests said so immediately: a capability
// default has to do SOMETHING harmless, so it returned ("", zero, false) — and
// every lease-freshness assertion then ran against a parser that never parsed.
//
// The tell is that this is not a capability at all. It reaches nothing; it is a
// pure function over a decoded object. `internal/converge` learned the same thing
// about LokiConfigText one extraction earlier: **a seam in the wrong place does
// not merely add indirection, it manufactures a vacuous fixture.**
//
// Sharing it is also the point. The reconciler WRITES this Lease and
// assert-reconciler JUDGES it; a second copy of the parse is exactly how an
// assertion and its subject drift apart, and the drift here is not cosmetic (see
// the renewOK contract below).

import "time"

// LeaseHolderRenew extracts holderIdentity and renewTime from a Lease object,
// defensive against missing/oddly-typed fields.
//
// renewOK is false when a renewTime value is PRESENT but unusable — not a
// string, or not parseable. That case must not be confused with an absent
// renewTime, because the caller treats a zero renewTime as "takeable NOW": the
// discarded parse error meant an unreadable timestamp read as evidence the lease
// was FREE, and a live holder's lease would be stolen out from under it. Two
// reconcilers then run every write lane at once, which is the exact thing leader
// election exists to prevent.
//
// The old comment ("RFC3339 parses the RFC3339Nano we write") is true only while
// WE are the sole writer. A Lease is a shared object — a manual kubectl edit, a
// serialization change, or another actor makes it false, and the failure mode is
// silent.
func LeaseHolderRenew(obj map[string]any) (holder string, renew time.Time, renewOK bool) {
	spec, _ := obj["spec"].(map[string]any)
	holder, _ = spec["holderIdentity"].(string)

	raw, present := spec["renewTime"]
	if !present || raw == nil {
		return holder, time.Time{}, true // genuinely never renewed — a real answer
	}
	rt, isStr := raw.(string)
	if !isStr || rt == "" {
		return holder, time.Time{}, false // present but not a usable timestamp
	}
	t, err := time.Parse(time.RFC3339, rt) // RFC3339 also accepts the RFC3339Nano we write
	if err != nil {
		return holder, time.Time{}, false
	}
	return holder, t, true
}
