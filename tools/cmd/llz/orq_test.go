package main

// orq_test.go — orQ is package main's, so its test stayed when the reap helpers
// went to internal/extensions/teardown with the sweep loops.

import "testing"

func TestOrQ(t *testing.T) {
	if got := orQ("v", true); got != "?" {
		t.Errorf("orQ(unknown) = %q, want \"?\"", got)
	}
	if got := orQ("v", false); got != "v" {
		t.Errorf("orQ(known) = %q, want \"v\"", got)
	}
}
