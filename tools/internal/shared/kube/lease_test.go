package kube

import "testing"

// LeaseHolderRenew moved here from package main's reconcile_leader.go so that the
// reconciler that WRITES the Lease and assert-reconciler that JUDGES it share one
// parse. It came without a direct test.
//
// The renewOK contract is the whole reason that matters, and it encodes a real
// incident: a renewTime that is PRESENT but unusable must not read as ABSENT,
// because the caller treats a zero renewTime as "takeable NOW". An unreadable
// timestamp reading as "lease is free" got a live holder's lease stolen, and two
// reconcilers then ran every write lane at once — the exact thing leader election
// exists to prevent.
func TestLeaseHolderRenew(t *testing.T) {
	lease := func(spec map[string]any) map[string]any {
		return map[string]any{"spec": spec}
	}

	// A well-formed Lease.
	h, r, ok := LeaseHolderRenew(lease(map[string]any{
		"holderIdentity": "pod-a",
		"renewTime":      "2026-08-06T12:00:00.000000Z",
	}))
	if h != "pod-a" || !ok || r.IsZero() {
		t.Errorf("well-formed lease = (%q, %v, %v), want pod-a / non-zero / true", h, r, ok)
	}

	// PRESENT BUT UNPARSEABLE is the incident. renewOK must be false, and the
	// caller must not be able to mistake it for an absent renewTime.
	if _, _, ok := LeaseHolderRenew(lease(map[string]any{
		"holderIdentity": "pod-a",
		"renewTime":      "not-a-timestamp",
	})); ok {
		t.Error("an unparseable renewTime reported renewOK=true — that is how a live " +
			"holder's lease gets stolen and two reconcilers run every write lane at once")
	}

	// Present but the WRONG TYPE — same class, different cause.
	if _, _, ok := LeaseHolderRenew(lease(map[string]any{
		"holderIdentity": "pod-a",
		"renewTime":      12345,
	})); ok {
		t.Error("a non-string renewTime reported renewOK=true")
	}

	// Absent spec / absent fields must be survivable, not a panic.
	if h, _, _ := LeaseHolderRenew(map[string]any{}); h != "" {
		t.Errorf("no spec = %q, want empty", h)
	}
	if h, _, _ := LeaseHolderRenew(lease(map[string]any{})); h != "" {
		t.Errorf("empty spec = %q, want empty", h)
	}
}
