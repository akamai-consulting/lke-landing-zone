package main

// Tier-1 coverage: table tests for the small PURE helpers that carried no direct
// test (they were only exercised incidentally through larger orchestrators).
// Each is deterministic on its inputs — no kubectl / API / filesystem.

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSetScalarChild(t *testing.T) {
	m := &yaml.Node{Kind: yaml.MappingNode}
	setScalarChild(m, "count", "3")      // append int
	setScalarChild(m, "enabled", "true") // append bool
	setScalarChild(m, "count", "8")      // replace in place

	if len(m.Content) != 4 {
		t.Fatalf("expected 4 content nodes (2 keys), got %d", len(m.Content))
	}
	got := map[string]*yaml.Node{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		got[m.Content[i].Value] = m.Content[i+1]
	}
	if got["count"].Value != "8" || got["count"].Tag != "!!int" {
		t.Errorf("count = (%q,%q), want (8, !!int)", got["count"].Value, got["count"].Tag)
	}
	if got["enabled"].Value != "true" || got["enabled"].Tag != "!!bool" {
		t.Errorf("enabled = (%q,%q), want (true, !!bool)", got["enabled"].Value, got["enabled"].Tag)
	}
}

func TestFilepathRel(t *testing.T) {
	if got := filepathRel("/a/b/cluster", "/a/b/prod.tfvars"); got != "prod.tfvars" {
		t.Errorf("filepathRel = %q, want prod.tfvars", got)
	}
	// Unrelatable paths fall back to dst unchanged.
	if got := filepathRel("rel/dir", "/abs/out"); got != "/abs/out" {
		t.Errorf("filepathRel fallback = %q, want /abs/out", got)
	}
}
