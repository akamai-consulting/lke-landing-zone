package main

import "testing"

// mostCommon feeds the generated import plan, so its answer must not depend on
// Go's randomized map iteration order. Ties go to the lexicographically
// smallest key; a run that let a later equal-count key win would emit a
// different plan on every invocation over the same cluster.
func TestMostCommonIsOrderIndependent(t *testing.T) {
	// 200 rounds: map order is re-randomized per range, so an order-dependent
	// winner shows up almost immediately.
	for i := 0; i < 200; i++ {
		if got := mostCommon(map[string]int{"alpha": 2, "beta": 2, "gamma": 2}); got != "alpha" {
			t.Fatalf("round %d: mostCommon = %q, want the lexicographically smallest of the tied keys", i, got)
		}
	}
	if got := mostCommon(map[string]int{"alpha": 2, "beta": 5}); got != "beta" {
		t.Errorf("mostCommon = %q, want the highest count", got)
	}
	if got := mostCommon(nil); got != "" {
		t.Errorf("mostCommon(nil) = %q, want empty", got)
	}
}
