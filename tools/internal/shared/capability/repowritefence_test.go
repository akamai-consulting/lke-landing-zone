package capability_test

// repowritefence_test.go — the RELATION between the two path fences.
//
// repo.Resolve and repoWriter.resolveForWrite each hold their own copy of one
// rule: "this path must land inside the tree". They are deliberately not the
// same function — a read may name a file that does not exist and a write may
// not follow a link out — but the difference is only ever allowed to run in one
// direction. A path the READER refuses must never be one the WRITER accepts.
//
// That direction was inverted for as long as the writer resolved only the
// parent. `root/out -> /etc/passwd` was refused by Repo.Resolve and permitted by
// RepoWriter.WriteFile, on a fence whose entire subject is mutation, under a
// header claiming the escape was closed.
//
// TestAWriteCannotEscapeThroughASymlinkedParent pins one layout and this pins
// the relation, because the defect was the divergence rather than the layout
// that revealed it: any future edit to either resolver that reopens a gap in
// this direction fails here without anyone having to think of the case.

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// fenceLayout builds one adversarial tree and names the path to try against it.
//
// readerRefuses records which of the two fences is expected to catch it, and it
// is ASSERTED rather than merely documented. Repo.Resolve deliberately lets a
// path that does not exist through — a guard must be able to report a missing
// file — so a symlinked PARENT plus an absent leaf is invisible to the reader
// and caught only by the writer's anchor. That is the writer being stricter,
// which is the safe direction. Pinning the flag both ways means a later change
// to either resolver reclassifies a row loudly instead of quietly turning the
// relation check vacuous.
type fenceLayout struct {
	name          string
	readerRefuses bool
	build         func(t *testing.T, root, outside string) string
}

var fenceLayouts = []fenceLayout{
	{
		// THE ONE THAT WAS OPEN. A link AT the target, to a file that exists.
		name:          "leaf symlink to an outside file",
		readerRefuses: true,
		build: func(t *testing.T, root, outside string) string {
			victim := filepath.Join(outside, "passwd")
			if err := os.WriteFile(victim, []byte("root:x:0:0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(victim, filepath.Join(root, "out")); err != nil {
				t.Fatal(err)
			}
			return "out"
		},
	},
	{
		name:          "leaf symlink to an outside directory",
		readerRefuses: true,
		build: func(t *testing.T, root, outside string) string {
			if err := os.Symlink(outside, filepath.Join(root, "outdir")); err != nil {
				t.Fatal(err)
			}
			return "outdir"
		},
	},
	{
		// Already closed by the parent anchor; kept so the relation is checked
		// across both halves of the resolver rather than only the new one.
		name: "symlinked parent, target does not exist",
		build: func(t *testing.T, root, outside string) string {
			if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
				t.Fatal(err)
			}
			return filepath.Join("out", "passwd")
		},
	},
	{
		name: "symlinked grandparent",
		build: func(t *testing.T, root, outside string) string {
			if err := os.MkdirAll(filepath.Join(outside, "d"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
				t.Fatal(err)
			}
			return filepath.Join("out", "d", "x")
		},
	},
	{
		name:          "lexical escape",
		readerRefuses: true,
		build: func(t *testing.T, root, outside string) string {
			return filepath.Join("..", filepath.Base(outside), "x")
		},
	},
	{
		name:          "absolute path",
		readerRefuses: true,
		build: func(t *testing.T, root, outside string) string {
			return filepath.Join(outside, "x")
		},
	},
}

// THE INVARIANT: refused-by-read implies refused-by-write. Both real resolvers
// are called — neither rule is restated here, which is the only way this can
// still be true after someone edits one of them.
func TestTheWriteFenceIsNeverWeakerThanTheRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on Windows")
	}
	for _, tc := range fenceLayouts {
		t.Run(tc.name, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			rel := tc.build(t, root, outside)

			b := binding(extension.WriteRepo)
			_, readErr := capability.RepoAt(b, root).Stat(rel)
			writeErr := capability.RepoWriterAt(b, root).PermitsWrite(rel)

			// EVERY ROW IS AN ESCAPE, so the writer refuses all of them. This is
			// the safety property; the relation below is what keeps it from
			// regressing by accident.
			if !errors.Is(writeErr, capability.ErrOutsideRepo) {
				t.Errorf("the writer permits %q (%v): this layout leaves the tree", rel, writeErr)
			}

			// Only a FENCE refusal counts as the reader saying no. A plain
			// "no such file" is the reader doing its job on a path that is
			// lexically inside the tree.
			gotRead := errors.Is(readErr, capability.ErrOutsideRepo)
			if gotRead != tc.readerRefuses {
				t.Errorf("reader refuses %q = %v, want %v (%v) — the row's classification is stale, "+
					"and a row that silently stops testing the relation is how the divergence lasted",
					rel, gotRead, tc.readerRefuses, readErr)
			}
		})
	}
}

// AND THE WRITE DOES NOT LAND. PermitsWrite refusing is the contract; this is
// the byte on disk, because the two came apart once already — the reader was
// correct about the same path the writer was overwriting.
func TestALeafSymlinkDoesNotCarryAWriteOutOfTheTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on Windows")
	}
	root, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "passwd")
	const original = "root:x:0:0\n"
	if err := os.WriteFile(victim, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	w := capability.RepoWriterAt(binding(extension.WriteRepo), root)

	if err := w.WriteFile("out", []byte("pwned"), 0o600); !errors.Is(err, capability.ErrOutsideRepo) {
		t.Errorf("WriteFile through a leaf symlink = %v, want ErrOutsideRepo", err)
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != original {
		t.Fatalf("the outside file was modified: (%q, %v)", got, err)
	}
	// EACH OPERATION ON ITS OWN TREE. Sharing one made the table order-dependent
	// the moment RemoveAll started succeeding: it unlinked the symlink, and the
	// rows after it were then asserting against a path that no longer existed.
	for _, tc := range []struct {
		name string
		call func(capability.RepoWriter) error
	}{
		{"MkdirAll", func(w capability.RepoWriter) error { return w.MkdirAll("out", 0o755) }},
		{"PermitsWrite", func(w capability.RepoWriter) error { return w.PermitsWrite("out") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "passwd"), []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "passwd"), filepath.Join(root, "out")); err != nil {
				t.Fatal(err)
			}
			w := capability.RepoWriterAt(binding(extension.WriteRepo), root)
			if err := tc.call(w); !errors.Is(err, capability.ErrOutsideRepo) {
				t.Errorf("%s through a leaf symlink = %v, want ErrOutsideRepo", tc.name, err)
			}
		})
	}
}

// REMOVEALL IS THE EXCEPTION, AND IT HAS TO BE. os.RemoveAll unlinks the LINK,
// so where the link points never comes into it — and refusing on that basis
// broke the one caller that prunes a tree: deliverdocs walks an adopter's docs/
// and returns on the first error, so a single outward or broken symlink in there
// aborted `llz ci deliver-docs`.
func TestRemoveAllUnlinksALeafSymlinkInsteadOfRefusingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on Windows")
	}
	root, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "passwd")
	const original = "root:x:0:0\n"
	if err := os.WriteFile(victim, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "gone"), filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	w := capability.RepoWriterAt(binding(extension.WriteRepo), root)

	for _, rel := range []string{"out", "dangling"} {
		if err := w.RemoveAll(rel); err != nil {
			t.Errorf("RemoveAll(%q) = %v, want nil — removing a link is not following it", rel, err)
		}
		if _, err := os.Lstat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Errorf("RemoveAll(%q) left the link in place: %v", rel, err)
		}
	}
	// AND THE TARGET SURVIVES, which is the whole reason this is safe.
	if got, err := os.ReadFile(victim); err != nil || string(got) != original {
		t.Fatalf("RemoveAll followed the link and deleted the outside file: (%q, %v)", got, err)
	}
	// The parent anchor still applies: a link ABOVE the target is refused, so
	// only the leaf is exempt.
	if err := os.Symlink(outside, filepath.Join(root, "outdir")); err != nil {
		t.Fatal(err)
	}
	if err := w.RemoveAll(filepath.Join("outdir", "passwd")); !errors.Is(err, capability.ErrOutsideRepo) {
		t.Errorf("RemoveAll THROUGH a symlinked parent = %v, want ErrOutsideRepo", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("the outside file was removed through a symlinked parent: %v", err)
	}
}

// A DANGLING LINK IS REFUSED TOO, and this is the arm most likely to be
// "simplified" away later: EvalSymlinks fails on it exactly as it does on a
// path that was never created, so the tempting read is that there is nothing
// there to protect. There is — a write through `root/x -> <outside>/new`
// CREATES the outside file.
func TestADanglingLeafSymlinkIsRefusedRatherThanFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on Windows")
	}
	root, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "new")
	if err := os.Symlink(target, filepath.Join(root, "x")); err != nil {
		t.Fatal(err)
	}
	w := capability.RepoWriterAt(binding(extension.WriteRepo), root)

	if err := w.WriteFile("x", []byte("pwned"), 0o600); !errors.Is(err, capability.ErrOutsideRepo) {
		t.Errorf("WriteFile through a dangling leaf symlink = %v, want ErrOutsideRepo", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("the write created a file outside the tree: %v", err)
	}
}

// THE FENCE STILL PASSES ORDINARY WORK. A tightened check that refuses the
// normal case gets reverted, so the permitted paths are pinned beside the
// refused ones — including an in-tree symlink, which is inside the tree and must
// keep working.
func TestTheTightenedFenceStillPermitsOrdinaryWrites(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on Windows")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real", "f"), filepath.Join(root, "inside")); err != nil {
		t.Fatal(err)
	}
	w := capability.RepoWriterAt(binding(extension.WriteRepo), root)

	for _, rel := range []string{
		"plain.txt",                          // does not exist yet
		filepath.Join("deep", "a", "b", "c"), // several levels into an empty tree
		filepath.Join("real", "f"),           // an existing regular file
		"inside",                             // a symlink that stays in the tree
	} {
		if err := w.PermitsWrite(rel); err != nil {
			t.Errorf("PermitsWrite(%q) = %v, want nil", rel, err)
		}
	}
	if err := w.WriteFile("inside", []byte("y"), 0o600); err != nil {
		t.Fatalf("writing through an in-tree symlink: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "real", "f")); err != nil || string(got) != "y" {
		t.Fatalf("the in-tree symlink write did not land: (%q, %v)", got, err)
	}
}
