package tokeninv

// TestCapitalizeFirst followed capitalizeFirst here; it was in package main's
// coverage_tier1_test.go, another file named for a coverage tier rather than a
// subject.

import "testing"

func TestCapitalizeFirst(t *testing.T) {
	for in, want := range map[string]string{"": "", "hello": "Hello", "A": "A", "123": "123"} {
		if got := capitalizeFirst(in); got != want {
			t.Errorf("capitalizeFirst(%q) = %q, want %q", in, got, want)
		}
	}
}
