package main

// Tier-1 coverage: table tests for the small PURE helpers that carried no direct
// test (they were only exercised incidentally through larger orchestrators).
// Each is deterministic on its inputs — no kubectl / API / filesystem.

import (
	"testing"
)

func TestFilepathRel(t *testing.T) {
	if got := filepathRel("/a/b/cluster", "/a/b/prod.tfvars"); got != "prod.tfvars" {
		t.Errorf("filepathRel = %q, want prod.tfvars", got)
	}
	// Unrelatable paths fall back to dst unchanged.
	if got := filepathRel("rel/dir", "/abs/out"); got != "/abs/out" {
		t.Errorf("filepathRel fallback = %q, want /abs/out", got)
	}
}
