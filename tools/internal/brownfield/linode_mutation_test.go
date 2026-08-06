package brownfield

import "testing"

// The pool listing is sorted by id so a re-run of `llz import` produces the
// same plan. Pools whose payload carries no "id" all map to 0, and the sort
// must leave those in the order the API returned them rather than permuting
// them — a comparator that calls equal ids "less" reorders an already-ordered
// run and the generated plan churns.
func TestLKENodePoolsKeepsAPIOrderForEqualIDs(t *testing.T) {
	out := lkeNodePools([]map[string]any{
		{"type": "g6-standard-4", "count": float64(3)}, // no id → 0
		{"type": "g6-standard-8", "count": float64(2)}, // no id → 0
	})
	if len(out) != 2 {
		t.Fatalf("pools = %d, want 2", len(out))
	}
	if out[0].Type != "g6-standard-4" || out[1].Type != "g6-standard-8" {
		t.Errorf("id-less pools must keep their API order, got %+v", out)
	}

	// The ordinary case: sorted ascending by id regardless of input order.
	out = lkeNodePools([]map[string]any{
		{"id": float64(9), "type": "b"},
		{"id": float64(3), "type": "a"},
	})
	if len(out) != 2 || out[0].ID != 3 || out[1].ID != 9 {
		t.Errorf("pools = %+v, want id-ascending", out)
	}
}

// Same determinism requirement one level up: firewalls are reported sorted by
// label, and two firewalls sharing a label must not swap places between runs.
func TestClusterFirewallsKeepsInputOrderForEqualLabels(t *testing.T) {
	fws := []map[string]any{
		{"id": float64(1), "label": "platform-nodes-fw"},
		{"id": float64(2), "label": "platform-nodes-fw"},
	}
	out := clusterFirewalls(fws, "", 0)
	if len(out) != 2 {
		t.Fatalf("firewalls = %d, want both matched by label", len(out))
	}
	if out[0].ID != 1 || out[1].ID != 2 {
		t.Errorf("equal labels must keep API order, got %+v", out)
	}

	// And the ordinary case: label-ascending regardless of input order.
	out = clusterFirewalls([]map[string]any{
		{"id": float64(1), "label": "zeta-lke-77"},
		{"id": float64(2), "label": "alpha-lke-77"},
	}, "", 77)
	if len(out) != 2 || out[0].Label != "alpha-lke-77" || out[1].Label != "zeta-lke-77" {
		t.Errorf("firewalls = %+v, want label-ascending", out)
	}
}

// The VPC match has three tiers: exact, "<label>-vpc", then the first label
// that merely CONTAINS the cluster label. That last tier is what finds an
// LKE-E VPC named after the cluster with a suffix, and "first" has to mean the
// first one seen — a later, equally-containing label must not displace it.
func TestMatchClusterVPCFallsBackToTheFirstContainingLabel(t *testing.T) {
	vpcs := []map[string]any{
		{"id": float64(1), "label": "unrelated"},
		{"id": float64(2), "label": "lke123-primary"}, // contains → the fallback
		{"id": float64(3), "label": "lke123-second"},  // also contains → must not win
	}
	got, ok := matchClusterVPC(vpcs, "lke123")
	if !ok {
		t.Fatal("a label containing the cluster label must match as a fallback")
	}
	if mapUint(got, "id") != 2 {
		t.Errorf("fallback picked id=%v, want the FIRST containing label (id 2)", got["id"])
	}

	// An exact / "-vpc" match still wins outright over a containing label.
	got, ok = matchClusterVPC([]map[string]any{
		{"id": float64(5), "label": "lke123-second"},
		{"id": float64(6), "label": "lke123-vpc"},
	}, "lke123")
	if !ok || mapUint(got, "id") != 6 {
		t.Errorf("exact -vpc match = %v (ok=%v), want id 6", got, ok)
	}

	if _, ok := matchClusterVPC(vpcs, ""); ok {
		t.Error("an empty cluster label must never match")
	}
}
