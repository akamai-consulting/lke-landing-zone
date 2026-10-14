package guardwalk

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// reader is the fenced reader Walk now takes — a bare read-repo binding, which is
// what every gate declares.
func reader(root string) capability.Repo {
	return capability.RepoAt(extension.Binding{
		Kind:   extension.Gate,
		State:  extension.Scaffolded,
		Grants: []extension.Grant{extension.ReadRepo},
	}, root)
}

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
	examined, err := Walk(
		reader(root),
		[]string{"a", "does-not-exist"},
		func(path string, raw []byte) error {
			seen = append(seen, filepath.Base(path))
			if len(raw) == 0 {
				t.Errorf("%s: contents not passed through", path)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("Walk: %v", err) // a missing dir must be skipped, not an error
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

// AN ABSENT TREE AND A TREE OUTSIDE THE FENCE ARE DIFFERENT ANSWERS, and the walk
// must not collapse them. The missing-dir skip exists because a guard runs over
// layouts where a given tree may legitimately be absent; a guard pointed OUT of
// the repository is a defect. Matching on "Stat failed" rather than on
// fs.ErrNotExist would silently walk past the second case and then fail the
// corpus check with the wrong reason.
func TestAnOutOfTreeDirIsRefusedNotSkipped(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "x.yaml"), []byte("kind: A\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Absent: skipped, no error, nothing examined.
	examined, err := Walk(reader(root), []string{"does-not-exist"}, func(string, []byte) error { return nil })
	if err != nil || examined != 0 {
		t.Errorf("an absent tree must be skipped: examined=%d err=%v", examined, err)
	}

	// Outside: refused, loudly.
	_, err = Walk(reader(root), []string{"../elsewhere"}, func(string, []byte) error { return nil })
	if !errors.Is(err, capability.ErrOutsideRepo) {
		t.Errorf("a dir outside the tree must be refused, got %v", err)
	}
}

// CollectPaths carries the fence too, and returns the paths found SO FAR
// alongside an error — a caller that drops the error must not also be handed an
// empty slice, since the guards read "no files" as the clean case.
func TestCollectPathsIsFencedAndYieldsReadablePaths(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "x.yaml"), []byte("kind: A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := reader(root)
	paths, err := CollectPaths(r, []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("want 1 path, got %v", paths)
	}
	// The path must go back into the reader that produced it. A walk yielding
	// paths its own ReadFile refuses is a fence that holds and a reader nobody
	// can use.
	if _, err := r.ReadFile(paths[0]); err != nil {
		t.Errorf("CollectPaths yielded %q, which the same reader refuses: %v", paths[0], err)
	}
}
