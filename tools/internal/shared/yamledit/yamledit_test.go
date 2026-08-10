package yamledit

// yamledit_test.go — the FENCED path, which is new surface rather than a
// re-test of the old one. EditSpecFile and EditSpecFileVia share one body on
// purpose: a duplicated edit-and-rollback is how the guard and the verbs would
// come to disagree about what an edit does, so these assert the Via path reaches
// the CALLER's reader and writer and never os.

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// fakeEditor records what a fenced caller would have written, so the Via path is
// exercised without a real tree — and, more to the point, proves the SAME body
// serves both callers. A forked copy is how the guard and the verbs would come to
// disagree about what an edit does.
type fakeEditor struct {
	files map[string][]byte
}

func (f *fakeEditor) ReadFile(p string) ([]byte, error) {
	b, ok := f.files[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return b, nil
}

func (f *fakeEditor) WriteFile(p string, b []byte, _ fs.FileMode) error {
	f.files[p] = append([]byte(nil), b...)
	return nil
}

func TestEditSpecFileViaUsesTheCallersReaderAndWriter(t *testing.T) {
	e := &fakeEditor{files: map[string][]byte{
		"spec.yaml": []byte("spec:\n  dns:\n    acmeEmail: old@x.com\n"),
	}}

	if err := EditSpecFileVia(e, "spec.yaml", func(doc *yaml.Node) error {
		return SetSpecPath(doc, "dns.acmeEmail", "new@x.com")
	}, func([]byte) error { return nil }); err != nil {
		t.Fatalf("EditSpecFileVia: %v", err)
	}
	if got := string(e.files["spec.yaml"]); !strings.Contains(got, "new@x.com") {
		t.Errorf("the edit did not reach the caller's writer:\n%s", got)
	}
	// Nothing escaped to the real filesystem.
	if _, err := os.Stat("spec.yaml"); err == nil {
		t.Error("EditSpecFileVia wrote through os, not through the caller's writer")
	}
}

// THE ROLLBACK GOES THROUGH THE CALLER'S WRITER TOO. If it did not, a fenced
// caller whose edit failed to parse would leave the poisoned file behind — the
// failure path nobody exercises, which is exactly why this asserts it.
func TestEditSpecFileViaRollsBackThroughTheSameWriter(t *testing.T) {
	orig := "spec:\n  dns:\n    acmeEmail: old@x.com\n"
	e := &fakeEditor{files: map[string][]byte{"spec.yaml": []byte(orig)}}

	err := EditSpecFileVia(e, "spec.yaml", func(doc *yaml.Node) error {
		return SetSpecPath(doc, "dns.acmeEmail", "new@x.com")
	}, func([]byte) error { return errors.New("rejected by the schema") })
	if err == nil {
		t.Fatal("a parse failure must be reported, not swallowed")
	}
	if got := string(e.files["spec.yaml"]); got != orig {
		t.Errorf("the file was not rolled back through the caller's writer:\ngot  %q\nwant %q", got, orig)
	}
}

// FencedEditor composes a separate reader and writer — the shape a caller holding
// capability.Repo and capability.RepoWriter has, since the two grants are
// separate handles.
func TestFencedEditorComposesTheTwoHalves(t *testing.T) {
	backing := &fakeEditor{files: map[string][]byte{"spec.yaml": []byte("spec:\n  a: 1\n")}}
	e := FencedEditor(backing, backing)

	b, err := e.ReadFile("spec.yaml")
	if err != nil || !strings.Contains(string(b), "a: 1") {
		t.Fatalf("read through the composed editor: (%q, %v)", b, err)
	}
	if err := e.WriteFile("spec.yaml", []byte("spec:\n  a: 2\n"), 0o644); err != nil {
		t.Fatalf("write through the composed editor: %v", err)
	}
	if got := string(backing.files["spec.yaml"]); !strings.Contains(got, "a: 2") {
		t.Errorf("the write did not reach the writer half: %s", got)
	}
}
