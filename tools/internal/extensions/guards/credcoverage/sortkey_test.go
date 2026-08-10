package credcoverage

// TestEsPropFilesSortKey followed esPropFiles here.
//
// FIFTH test this branch has found stranded in package main's
// sortkey_test.go — a file named for a COVERAGE TIER rather than a
// subject, which is why nothing about it ever suggests which code it belongs to.
// The pattern is now firm enough to state as a rule: files named for a metric
// collect tests that have no home, and every extraction has to grep for moved
// SYMBOLS rather than trusting moved files.

import "testing"

func TestEsPropFilesSortKey(t *testing.T) {
	if got := (esPropFiles{prop: "secret/x", hasProp: true}).sortKey(); got != "secret/x" {
		t.Errorf("sortKey(hasProp) = %q, want secret/x", got)
	}
	if got := (esPropFiles{prop: "secret/x", hasProp: false}).sortKey(); got != "" {
		t.Errorf("sortKey(!hasProp) = %q, want empty", got)
	}
}
