package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWalkManifestsEndsTheDivergences pins the three behaviors the five copies of
// this walk disagreed on before it was shared.
func TestWalkManifestsEndsTheDivergences(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a/one.yaml", "kind: A\n")
	write("a/two.yml", "kind: B\n")             // three of the five guards ignored this
	write("a/templates/tpl.yaml", "{{ .x }}\n") // Go-templated, not a manifest
	write("a/notes.md", "not yaml\n")

	var seen []string
	examined, err := walkManifests(
		[]string{filepath.Join(root, "a"), filepath.Join(root, "does-not-exist")},
		func(path string, raw []byte) error {
			seen = append(seen, filepath.Base(path))
			if len(raw) == 0 {
				t.Errorf("%s: contents not passed through", path)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("walkManifests: %v", err) // a missing dir must be skipped, not an error
	}
	got := strings.Join(seen, ",")
	if !strings.Contains(got, "one.yaml") || !strings.Contains(got, "two.yml") {
		t.Errorf("walked %q, want BOTH .yaml and .yml — the extension split is what let a *.yml manifest be policed by one guard and invisible to three", got)
	}
	if strings.Contains(got, "tpl.yaml") {
		t.Errorf("walked %q, must skip templates/ (Go-templated YAML is not a manifest)", got)
	}
	if strings.Contains(got, "notes.md") {
		t.Errorf("walked %q, must skip non-YAML", got)
	}
	if examined != 2 {
		t.Errorf("examined = %d, want 2 — the count requireCorpus gates on must reflect files actually read", examined)
	}
}
