package main

// Tier-1 coverage: table tests for the small PURE helpers that carried no direct
// test (they were only exercised incidentally through larger orchestrators).
// Each is deterministic on its inputs — no kubectl / API / filesystem.

import (
	"os"
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

func TestInstanceLayout(t *testing.T) {
	t.Chdir(t.TempDir())

	// Rendered instance: roots at repo root.
	tf, apl, prefix := instanceLayout()
	if tf != "terraform-iac-bootstrap" || apl != "apl-values" || prefix != "" {
		t.Errorf("rendered layout = (%q,%q,%q)", tf, apl, prefix)
	}

	// Template-repo checkout: roots under instance-template/.
	if err := os.MkdirAll("instance-template/terraform-iac-bootstrap", 0o755); err != nil {
		t.Fatal(err)
	}
	tf, apl, prefix = instanceLayout()
	if tf != "instance-template/terraform-iac-bootstrap" || apl != "instance-template/apl-values" || prefix != "instance-template/" {
		t.Errorf("template layout = (%q,%q,%q)", tf, apl, prefix)
	}
}

func TestLiveStateValue(t *testing.T) {
	s := liveState{
		envVars:  map[string]string{"A": "env"},
		repoVars: map[string]string{"A": "repo", "B": "only-repo"},
	}
	if v := s.value("A"); v != "env" { // env scope wins
		t.Errorf("value(A) = %q, want env", v)
	}
	if v := s.value("B"); v != "only-repo" {
		t.Errorf("value(B) = %q, want only-repo", v)
	}
	if v := s.value("missing"); v != "" {
		t.Errorf("value(missing) = %q, want empty", v)
	}
}

func TestEsPropFilesSortKey(t *testing.T) {
	if got := (esPropFiles{prop: "secret/x", hasProp: true}).sortKey(); got != "secret/x" {
		t.Errorf("sortKey(hasProp) = %q, want secret/x", got)
	}
	if got := (esPropFiles{prop: "secret/x", hasProp: false}).sortKey(); got != "" {
		t.Errorf("sortKey(!hasProp) = %q, want empty", got)
	}
}
