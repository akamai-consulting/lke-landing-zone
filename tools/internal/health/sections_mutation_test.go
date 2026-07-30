package health

import "testing"

// sections_mutation_test.go pins two section verdict boundaries: the NetworkPolicy
// count that clears a namespace, and the age at which a Terminating object counts
// as stuck.

// TestClassifyNamespaceNetpol_OnePolicyPasses pins the count boundary. Exactly one
// default-deny NetworkPolicy is the normal case for a repo-managed namespace —
// requiring more than one fails every correctly-configured namespace.
func TestClassifyNamespaceNetpol_OnePolicyPasses(t *testing.T) {
	cases := []struct {
		count int
		want  Category
	}{
		{0, CatFail},
		{1, CatOK},
		{2, CatOK},
		{7, CatOK},
	}
	for _, c := range cases {
		got, msg := ClassifyNamespaceNetpol("observability", c.count)
		if got != c.want {
			t.Errorf("ClassifyNamespaceNetpol(observability, %d) = %v (%q), want %v", c.count, got, msg, c.want)
		}
	}
}

// TestStuckFinalizer_AgeBoundary pins the 5-minute Terminating threshold: exactly
// 300s is NOT yet stuck (a deletion in flight), anything past it is. This is the
// heuristic that turns a normal delete into a reported failure, so the boundary
// itself is the contract.
func TestStuckFinalizer_AgeBoundary(t *testing.T) {
	cases := []struct {
		age  float64
		want bool
	}{
		{0, false},
		{299.9, false},
		{300, false}, // exactly at the threshold — still a deletion in flight
		{300.1, true},
		{301, true},
		{600, true},
	}
	for _, c := range cases {
		if got := StuckFinalizer(true, 1, c.age); got != c.want {
			t.Errorf("StuckFinalizer(true, 1, %v) = %v, want %v", c.age, got, c.want)
		}
	}
	// The other two conjuncts still gate an over-age object.
	if StuckFinalizer(false, 1, 600) {
		t.Error("no deletionTimestamp is not stuck regardless of age")
	}
	if StuckFinalizer(true, 0, 600) {
		t.Error("no finalizers is not stuck regardless of age")
	}
}
