package yamledit

// Tests that followed their subjects here. inferScalarTag and setScalarChild are
// the scalar-writing half of the comment-preserving editor; their tests were in
// internal/envtopology's env_set_test.go and package main's coverage_tier1_test.go
// respectively — the ninth and tenth this branch has found separated from the code
// they cover.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

func TestInferScalarTag(t *testing.T) {
	for v, want := range map[string]string{"true": "!!bool", "false": "!!bool", "8": "!!int", "30d": "!!str", "us-ord": "!!str", "10.0.0.0/14": "!!str"} {
		if got := inferScalarTag(v); got != want {
			t.Errorf("inferScalarTag(%q) = %q, want %q", v, got, want)
		}
	}
}

// #3: an HA group with only one peer reports the missing role; complete → "".
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
func TestEditYAMLFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(path, []byte("cluster:\n  name: old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := EditYAMLFile(path, func(doc *yaml.Node) error {
		setScalarChild(doc.Content[0], "added", "yes")
		return nil
	})
	if err != nil {
		t.Fatalf("editYAMLFile: %v", err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "added: yes") {
		t.Errorf("mutation not written:\n%s", b)
	}

	// Error paths.
	if err := EditYAMLFile(filepath.Join(dir, "nope.yaml"), func(*yaml.Node) error { return nil }); err == nil {
		t.Error("missing file should error")
	}
	empty := filepath.Join(dir, "empty.yaml")
	os.WriteFile(empty, nil, 0o644)
	if err := EditYAMLFile(empty, func(*yaml.Node) error { return nil }); err == nil {
		t.Error("empty doc should error")
	}
	sentinel := errors.New("mutate failed")
	if err := EditYAMLFile(path, func(*yaml.Node) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("mutate error should propagate, got %v", err)
	}
}
