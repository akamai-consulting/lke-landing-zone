package capability_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func tree(t *testing.T) (root string, r capability.Repo) {
	t.Helper()
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, capability.RepoAt(binding(extension.ReadRepo), root)
}

// THE FENCE, AND THE THREE WAYS OUT OF IT.
//
// This is the property `read-repo` did not have: 40 extensions declared it, the
// validator refused a gate that declared anything ELSE, and nothing stopped any of
// them reading ~/.aws/credentials. The claim that `llz ci gates` "touches nothing
// but files" was a check on the declaration.
func TestTheFenceRefusesEveryWayOut(t *testing.T) {
	_, r := tree(t)

	for _, tc := range []struct{ name, rel string }{
		{"parent traversal", "../outside.txt"},
		{"traversal after a legitimate segment", "docs/../../outside.txt"},
		{"bare parent", ".."},
		// filepath.Join(root, "/etc/passwd") is root/etc/passwd — it does NOT
		// escape, so a containment check PASSES and the caller silently reads the
		// wrong file. The bug is believing the path means what it says.
		{"absolute path", "/etc/passwd"},
	} {
		if err := r.Permits(tc.rel); !errors.Is(err, capability.ErrOutsideRepo) {
			t.Errorf("%s (%q): Permits = %v, want ErrOutsideRepo", tc.name, tc.rel, err)
		}
		if _, err := r.ReadFile(tc.rel); err == nil {
			t.Errorf("%s (%q): ReadFile SUCCEEDED", tc.name, tc.rel)
		}
	}

	if err := r.Permits("inside.txt"); err != nil {
		t.Errorf("a legitimate path was refused: %v", err)
	}
	b, err := r.ReadFile("inside.txt")
	if err != nil || string(b) != "ok" {
		t.Errorf("ReadFile(inside.txt) = (%q, %v)", b, err)
	}
}

// A SYMLINK INSIDE THE TREE POINTING OUTSIDE IT passes every lexical check —
// Clean sees nothing wrong. This is the case a string-comparison fence misses, and
// this repo's own trees contain links.
func TestASymlinkOutOfTheTreeIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on Windows")
	}
	root, r := tree(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("password"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}

	// Lexically fine — which is the point.
	if err := r.Permits("link.txt"); err != nil {
		t.Fatalf("Permits should not reject lexically: %v", err)
	}
	if _, err := r.ReadFile("link.txt"); !errors.Is(err, capability.ErrOutsideRepo) {
		t.Errorf("read through a symlink out of the tree: err = %v", err)
	}
}

// A walk is where the symlink case actually bites: the root is legitimate and one
// entry inside it points elsewhere. The escape is SKIPPED rather than fataling —
// a guard scanning a tree with one outward link should report on the rest.
func TestWalkSkipsAnEscapingEntryRatherThanFailing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on Windows")
	}
	root, r := tree(t)
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outDir, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	var seen []string
	err := r.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && d != nil && !d.IsDir() {
			seen = append(seen, filepath.Base(p))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk failed instead of skipping: %v", err)
	}
	for _, s := range seen {
		if s == "secret.txt" {
			t.Error("the walk handed the callback a file outside the tree")
		}
	}
	var sawInside bool
	for _, s := range seen {
		if s == "inside.txt" {
			sawInside = true
		}
	}
	if !sawInside {
		t.Error("the walk skipped the legitimate file too — an escape must not abort the scan")
	}
}

// A missing file is NOT a fence violation. Several guards exist to report "this
// file is named in the docs and does not exist"; refusing unresolvable paths would
// make that finding impossible to produce.
func TestAMissingFileIsAReadErrorNotAFenceError(t *testing.T) {
	_, r := tree(t)
	if err := r.Permits("does/not/exist.txt"); err != nil {
		t.Errorf("a missing path was refused by the FENCE: %v", err)
	}
	_, err := r.ReadFile("does/not/exist.txt")
	if errors.Is(err, capability.ErrOutsideRepo) {
		t.Error("a missing file reported as a fence violation — conflating the two is how a " +
			"fence gets `fixed` by creating the file")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("want a not-exist error, got %v", err)
	}
}

// `/repo-backup` must not count as inside `/repo`. A prefix test accepts it;
// comparing path SEGMENTS does not.
func TestASiblingDirectoryWithASharedPrefixIsOutside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on Windows")
	}
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	sibling := filepath.Join(base, "repo-backup")
	for _, d := range []string{root, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sibling, "x.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sibling, filepath.Join(root, "peek")); err != nil {
		t.Fatal(err)
	}

	r := capability.RepoAt(binding(extension.ReadRepo), root)
	if _, err := r.ReadFile("peek/x.txt"); !errors.Is(err, capability.ErrOutsideRepo) {
		t.Errorf("read into a sibling sharing the root's prefix: err = %v", err)
	}
}

// No read-repo, no reader — present and refusing, never nil.
func TestABindingWithoutReadRepoGetsARefusingReader(t *testing.T) {
	r := capability.RepoAt(binding(extension.ClusterRead), t.TempDir())
	if r == nil {
		t.Fatal("nil reader")
	}
	if err := r.Permits("anything"); !errors.Is(err, capability.ErrNoRepoRead) {
		t.Errorf("Permits = %v, want ErrNoRepoRead", err)
	}
	if _, err := r.ReadFile("anything"); err == nil {
		t.Error("ReadFile succeeded without read-repo")
	}
	if _, err := capability.DeniedRepo().ReadFile("x"); err == nil {
		t.Error("DeniedRepo().ReadFile succeeded")
	}
}

// write-repo implies the ability to read, as cluster-write and secret-custody do:
// every path that writes a file reads it back or reads its neighbours.
func TestWriteRepoImpliesRead(t *testing.T) {
	r := capability.RepoAt(binding(extension.WriteRepo), t.TempDir())
	if err := r.Permits("x.txt"); err != nil {
		t.Errorf("write-repo was refused a read: %v", err)
	}
}

// Stat and Resolve carry the same fence as ReadFile. Asserted separately because
// each applies it itself — a fence that only one entry point checks is a fence
// with three doors and one lock.
func TestEveryEntryPointCarriesTheFence(t *testing.T) {
	root, r := tree(t)

	for _, esc := range []string{"../outside", "/etc/passwd"} {
		if _, err := r.Stat(esc); !errors.Is(err, capability.ErrOutsideRepo) {
			t.Errorf("Stat(%q) = %v, want ErrOutsideRepo", esc, err)
		}
		if _, err := r.Resolve(esc); !errors.Is(err, capability.ErrOutsideRepo) {
			t.Errorf("Resolve(%q) = %v, want ErrOutsideRepo", esc, err)
		}
		if err := r.WalkDir(esc, func(string, fs.DirEntry, error) error { return nil }); !errors.Is(err, capability.ErrOutsideRepo) {
			t.Errorf("WalkDir(%q) = %v, want ErrOutsideRepo", esc, err)
		}
	}

	fi, err := r.Stat("inside.txt")
	if err != nil || fi.Name() != "inside.txt" {
		t.Errorf("Stat(inside.txt) = (%v, %v)", fi, err)
	}
	// Compared against the RESOLVED root: on macOS /var is a symlink to
	// /private/var, so t.TempDir() hands back an alias while Resolve — correctly —
	// returns the real path. Comparing the two forms directly is a test that fails
	// on a platform rather than on a defect.
	p, err := r.Resolve("inside.txt")
	if err != nil {
		t.Fatalf("Resolve(inside.txt): %v", err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != realRoot {
		t.Errorf("Resolve(inside.txt) = %q, want a path under %q", p, realRoot)
	}

	// The denied handle refuses at every door too.
	d := capability.DeniedRepo()
	if _, err := d.Stat("x"); err == nil {
		t.Error("DeniedRepo().Stat succeeded")
	}
	if _, err := d.Resolve("x"); err == nil {
		t.Error("DeniedRepo().Resolve succeeded")
	}
	if err := d.WalkDir(".", func(string, fs.DirEntry, error) error { return nil }); err == nil {
		t.Error("DeniedRepo().WalkDir succeeded")
	}
}

// THE WALK MUST YIELD PATHS ITS OWN READER ACCEPTS. This is the defect the first
// real corpus found: WalkDir handed the callback RESOLVED on-disk paths, which
// are absolute, and every entry point here refuses an absolute path. The fence
// held perfectly and the reader was unusable — a walk whose every result was
// rejected by the ReadFile two lines below it.
//
// It is asserted as a round trip rather than as a string shape because the string
// shape is not the point: what callers need is that a path out of the walk goes
// back into the handle.
func TestWalkYieldsPathsTheReaderAccepts(t *testing.T) {
	root, r := tree(t)
	if err := os.MkdirAll(filepath.Join(root, "sub", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "deeper", "nested.txt"), []byte("deep"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, start := range []string{".", "sub"} {
		var files int
		err := r.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return err
			}
			files++
			if _, rerr := r.ReadFile(p); rerr != nil {
				t.Errorf("WalkDir(%q) yielded %q, which the same handle refuses to read: %v",
					start, p, rerr)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("WalkDir(%q): %v", start, err)
		}
		if files == 0 {
			t.Errorf("WalkDir(%q) visited no files", start)
		}
	}

	// The path is expressed under the subtree the caller asked for, so a finding
	// names the file the way the caller would.
	var seen []string
	if err := r.WalkDir("sub", func(p string, d fs.DirEntry, err error) error {
		if err == nil && d != nil && !d.IsDir() {
			seen = append(seen, p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("sub", "deeper", "nested.txt")
	if len(seen) != 1 || seen[0] != want {
		t.Errorf("WalkDir(sub) yielded %v, want [%s]", seen, want)
	}
}

// A walk that hits a genuine I/O error must pass it to the callback rather than
// swallowing it — a guard that cannot read a file needs to say so, and silently
// skipping is how a corpus check reports clean over an unreadable tree.
func TestWalkPassesRealErrorsToTheCallback(t *testing.T) {
	_, r := tree(t)
	var sawErr bool
	_ = r.WalkDir("nope", func(_ string, _ fs.DirEntry, err error) error {
		if err != nil {
			sawErr = true
		}
		return nil
	})
	if !sawErr {
		t.Error("a walk of a missing subtree reported no error to the callback")
	}
}

// ReadDir CARRIES THE FENCE AND DROPS THE ESCAPES. Listing is the one operation
// where refusing the whole call would be wrong: a tree holding a single outward
// symlink is still a tree worth listing, so the entry goes and the directory
// stays.
func TestReadDirIsFencedPerEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on Windows")
	}
	root, r := tree(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("password"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "leak.txt")); err != nil {
		t.Fatal(err)
	}

	entries, err := r.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir(.): %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "inside.txt" {
		t.Errorf("ReadDir returned %v — want just inside.txt, with the outward symlink dropped", names)
	}

	// Every entry it DOES return must be readable through the same handle.
	for _, e := range entries {
		if _, err := r.ReadFile(e.Name()); err != nil {
			t.Errorf("ReadDir yielded %q, which the same handle refuses to read: %v", e.Name(), err)
		}
	}

	// The fence applies to the directory argument too.
	if _, err := r.ReadDir("../elsewhere"); !errors.Is(err, capability.ErrOutsideRepo) {
		t.Errorf("ReadDir out of the tree = %v, want ErrOutsideRepo", err)
	}
	if _, err := capability.DeniedRepo().ReadDir("."); !errors.Is(err, capability.ErrNoRepoRead) {
		t.Errorf("DeniedRepo().ReadDir = %v, want ErrNoRepoRead", err)
	}
}

// RepoContaining is for the callers whose flag names a TREE TO SCAN rather than a
// repo root. It must fence at the CONTAINING tree, not at the scan dir itself:
// fencing tighter would strip the segment the operator typed off every finding,
// so `rendered/x.yaml` would come back as `x.yaml` and two scan roots would
// produce colliding names.
func TestRepoContainingKeepsTheOperatorsSegment(t *testing.T) {
	base := t.TempDir()
	scan := filepath.Join(base, "rendered")
	if err := os.MkdirAll(scan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scan, "x.yaml"), []byte("kind: X"), 0o600); err != nil {
		t.Fatal(err)
	}

	r, rel := capability.RepoContaining(binding(extension.ReadRepo), scan)
	if rel != "rendered" {
		t.Errorf("rel = %q, want the segment the caller named (%q)", rel, "rendered")
	}
	got, readErr := r.ReadFile(filepath.Join(rel, "x.yaml"))
	if readErr != nil || string(got) != "kind: X" {
		t.Errorf("reading through the returned reader: (%q, %v)", got, readErr)
	}

	// A RELATIVE scan dir is already expressed under the working directory, so
	// that is the tree, and the dir is handed back unchanged.
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	r2, rel2 := capability.RepoContaining(binding(extension.ReadRepo), "rendered")
	if rel2 != "rendered" {
		t.Errorf("relative scan dir: rel = %q, want %q", rel2, "rendered")
	}
	if _, err := r2.Stat(filepath.Join(rel2, "x.yaml")); err != nil {
		t.Errorf("relative scan dir: %v", err)
	}

	// No grant, no reader — even though the path arithmetic still works.
	denied, _ := capability.RepoContaining(binding(extension.ClusterRead), scan)
	if err := denied.Permits("rendered"); !errors.Is(err, capability.ErrNoRepoRead) {
		t.Errorf("RepoContaining without read-repo = %v, want ErrNoRepoRead", err)
	}
}

// RepoForGate must build from the DECLARATION. A guard that could mint its own
// binding would be granting itself the capability, which is exactly the
// "grants annotate rather than constrain" state this layer exists to end.
func TestRepoForGateBuildsFromTheDeclaration(t *testing.T) {
	root, _ := treeRoot(t)

	declared := extension.Extension{Name: "guard-x", Bindings: []extension.Binding{{
		Kind: extension.Gate, State: extension.Scaffolded,
		Grants: []extension.Grant{extension.ReadRepo},
	}}}
	if _, err := capability.RepoForGate(declared, root).ReadFile("inside.txt"); err != nil {
		t.Errorf("a declared gate binding was refused its own tree: %v", err)
	}

	// A gate binding that declares nothing gets a refusing reader, not a
	// permissive one — the lookup does not imply the grant.
	stripped := extension.Extension{Name: "guard-y", Bindings: []extension.Binding{{
		Kind: extension.Gate, State: extension.Scaffolded,
	}}}
	if err := capability.RepoForGate(stripped, root).Permits("inside.txt"); !errors.Is(err, capability.ErrNoRepoRead) {
		t.Errorf("a gate declaring no grants got a reading handle: %v", err)
	}

	// A NON-gate binding is not picked up by mistake: an extension whose only
	// binding is an assertion has no gate, and that is a wiring bug rather than a
	// silent fallback to the assertion's grants.
	t.Run("no gate binding panics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("RepoForGate returned rather than panicking for an extension with no gate binding")
			}
		}()
		capability.RepoForGate(extension.Extension{
			Name:     "guard-z",
			Bindings: []extension.Binding{binding(extension.ReadRepo)},
		}, root)
	})
}

// treeRoot is tree() when only the root is wanted.
func treeRoot(t *testing.T) (string, capability.Repo) { return tree(t) }

// THE ASCENT IS THE ROOT. This is the defect `llz ci gates` caught on the first
// real run: a first cut fenced a relative scan dir at the working directory,
// reasoning that a relative path is already under it — true of `rendered`, false
// of anything beginning `..`. The driver invokes these gates from tools/ as
// `--root ..` and `--dir ../.github/workflows`, so both refused their own corpus.
//
// Asserted as a round trip rather than as a string shape: what matters is that
// the dir the caller named is readable through the reader it was handed.
func TestRepoContainingFencesAtTheAscent(t *testing.T) {
	base := t.TempDir()
	// Lay out a repo with a tools/ the gates run from, exactly as the driver does.
	for _, d := range []string{"tools", ".github/workflows", "rendered", "tools/rendered"} {
		if err := os.MkdirAll(filepath.Join(base, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{".github/workflows/ci.yml", "rendered/x.yaml"} {
		if err := os.WriteFile(filepath.Join(base, filepath.FromSlash(f)), []byte("kind: X"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(filepath.Join(base, "tools")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	for _, tc := range []struct{ name, dir, wantRel, readable string }{
		// The two shapes the gate driver actually passes.
		{"ascending scan dir", "../.github/workflows", ".github/workflows", "ci.yml"},
		{"bare ascent", "..", ".", "rendered/x.yaml"},
		// And the shape that already worked, which must keep working.
		{"no ascent", "rendered", "rendered", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, rel := capability.RepoContaining(binding(extension.ReadRepo), tc.dir)
			if rel != tc.wantRel {
				t.Errorf("rel = %q, want %q", rel, tc.wantRel)
			}
			if _, err := r.Stat(rel); err != nil {
				t.Fatalf("the reader cannot see the dir it was fenced for: %v", err)
			}
			if tc.readable != "" {
				if _, err := r.ReadFile(filepath.Join(rel, tc.readable)); err != nil {
					t.Errorf("reading %s through the returned reader: %v", tc.readable, err)
				}
			}
		})
	}
}

// RepoContainingAll fences a LIST under ONE reader, high enough for every entry.
// Sharing the reader is what keeps two scan roots from colliding: fenced
// separately, platform-apl/x.yaml and rendered/x.yaml both come back as x.yaml.
func TestRepoContainingAllSharesOneFenceHighEnoughForEvery(t *testing.T) {
	base := t.TempDir()
	for _, d := range []string{"tools", "platform-apl", "rendered"} {
		if err := os.MkdirAll(filepath.Join(base, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(filepath.Join(base, "tools")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	// Mixed depths: the fence must go at the DEEPEST ascent, not the first one.
	r, dirs := capability.RepoContainingAll(binding(extension.ReadRepo),
		[]string{"../platform-apl", "../rendered"})
	if len(dirs) != 2 || dirs[0] != "platform-apl" || dirs[1] != "rendered" {
		t.Fatalf("dirs = %v, want the segments the caller named", dirs)
	}
	for _, d := range dirs {
		if _, err := r.Stat(d); err != nil {
			t.Errorf("the shared reader cannot see %q: %v", d, err)
		}
	}

	// No dirs at all is not a crash: the reader is fenced at the working
	// directory and the list comes back empty.
	if r, dirs := capability.RepoContainingAll(binding(extension.ReadRepo), nil); r == nil || len(dirs) != 0 {
		t.Errorf("empty input: r=%v dirs=%v", r, dirs)
	}
}

// ── write-repo ───────────────────────────────────────────────────────────────

// THE ESCAPE A READ CANNOT MAKE. Repo.Resolve lets a non-existent path through
// on purpose (a guard must be able to report a missing file), which for a write
// means a link to /etc plus a filename that does not exist yet resolves to
// nothing checkable — and os.WriteFile follows the link. Resolving the PARENT is
// what closes it, and this is the test that says so.
func TestAWriteCannotEscapeThroughASymlinkedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	w := capability.RepoWriterAt(binding(extension.WriteRepo), root)

	// The target does not exist, so nothing about it can be resolved — the
	// lexical fence sees a clean relative path and the read fence would let it by.
	victim := filepath.Join("out", "passwd")
	if err := w.WriteFile(victim, []byte("pwned"), 0o600); !errors.Is(err, capability.ErrOutsideRepo) {
		t.Errorf("WriteFile through a symlinked parent = %v, want ErrOutsideRepo", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "passwd")); err == nil {
		t.Fatal("the write landed outside the tree")
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"MkdirAll", w.MkdirAll(filepath.Join("out", "d"), 0o755)},
		{"RemoveAll", w.RemoveAll(victim)},
		{"PermitsWrite", w.PermitsWrite(victim)},
	} {
		if !errors.Is(tc.err, capability.ErrOutsideRepo) {
			t.Errorf("%s through a symlinked parent = %v, want ErrOutsideRepo", tc.name, tc.err)
		}
	}
}

// A permitted write REACHES THE DISK, including several directories deep into an
// empty tree — MkdirAll must not be refused for want of a parent to resolve.
func TestAPermittedWriteReachesTheDisk(t *testing.T) {
	root := t.TempDir()
	w := capability.RepoWriterAt(binding(extension.WriteRepo), root)

	if err := w.MkdirAll(filepath.Join("a", "b", "c"), 0o755); err != nil {
		t.Fatalf("MkdirAll several levels into an empty tree: %v", err)
	}
	rel := filepath.Join("a", "b", "c", "x.txt")
	if err := w.WriteFile(rel, []byte("hello"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil || string(got) != "hello" {
		t.Fatalf("on disk: (%q, %v)", got, err)
	}
	// And the reader from the same binding can read it back.
	if b, err := capability.RepoAt(binding(extension.WriteRepo), root).ReadFile(rel); err != nil || string(b) != "hello" {
		t.Errorf("write-repo must imply read: (%q, %v)", b, err)
	}
	if err := w.RemoveAll(filepath.Join("a", "b")); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a", "b")); !os.IsNotExist(err) {
		t.Error("RemoveAll did not delete the subtree")
	}
}

// READ-REPO DOES NOT IMPLY WRITE-REPO. The implication runs one way, which is the
// entire reason the vocabulary has two words.
func TestReadRepoDoesNotConferWrite(t *testing.T) {
	root := t.TempDir()
	w := capability.RepoWriterAt(binding(extension.ReadRepo), root)
	if err := w.WriteFile("x.txt", []byte("x"), 0o600); !errors.Is(err, capability.ErrNoRepoWrite) {
		t.Errorf("a read-repo binding wrote a file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "x.txt")); err == nil {
		t.Fatal("the file was created despite the refusal")
	}
}

// A REFUSED RemoveAll MUST NOT LOOK LIKE A COMPLETED ONE. os.RemoveAll answers an
// absent path with nil, so a refusal returning nil would make a denied prune
// indistinguishable from a finished one — and deliver-docs prunes a tree. Same
// argument as deniedSecrets.Get returning Unknown rather than Absent.
func TestADeniedRemoveAllIsAnErrorNotANoOp(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    capability.RepoWriter
	}{
		{"no grant", capability.RepoWriterAt(binding(extension.ReadRepo), t.TempDir())},
		{"exported denied handle", capability.DeniedRepoWriter()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.w.RemoveAll("anything"); err == nil {
				t.Error("RemoveAll returned nil — a denied prune reads as a completed one")
			}
			if err := tc.w.MkdirAll("d", 0o755); err == nil {
				t.Error("MkdirAll returned nil")
			}
			if err := tc.w.PermitsWrite("x"); !errors.Is(err, capability.ErrNoRepoWrite) {
				t.Errorf("PermitsWrite = %v, want ErrNoRepoWrite", err)
			}
		})
	}
	if capability.DeniedRepoWriter() == nil {
		t.Error("DeniedRepoWriter() is nil")
	}
}

// The lexical half of the fence applies to writes too.
func TestWritesRefuseTheLexicalEscapes(t *testing.T) {
	w := capability.RepoWriterAt(binding(extension.WriteRepo), t.TempDir())
	for _, rel := range []string{"../outside.txt", "docs/../../outside.txt", "/etc/passwd"} {
		if err := w.WriteFile(rel, []byte("x"), 0o600); !errors.Is(err, capability.ErrOutsideRepo) {
			t.Errorf("WriteFile(%q) = %v, want ErrOutsideRepo", rel, err)
		}
	}
}

// RelTo is the conversion every converted writer needs, and the two forms it has
// to survive are the two that broke it: a mixed absolute/relative spelling, and a
// target whose directory does not exist yet. Both were real failures — the second
// refused every write `llz render` makes, because MkdirAll had not run.
func TestRelToSurvivesMixedSpellingsAndAbsentTargets(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	for _, tc := range []struct{ name, root, p, want string }{
		// Both relative — the production shape (instancelayout.Detect()).
		{"both relative", ".", filepath.Join("sub", "x.txt"), filepath.Join("sub", "x.txt")},
		// Absolute path against a relative root: filepath.Rel alone refuses these,
		// and on macOS the /var vs /private/var alias makes the naive fix produce
		// a ../.. chain out of the tree.
		{"absolute path, relative root", ".", filepath.Join(sub, "x.txt"), filepath.Join("sub", "x.txt")},
		{"both absolute", root, filepath.Join(sub, "x.txt"), filepath.Join("sub", "x.txt")},
		// The target's DIRECTORY does not exist yet — MkdirAll has not run. This
		// is the case that refused every render write.
		{"absent directory", root, filepath.Join(root, "nope", "deep", "x.txt"),
			filepath.Join("nope", "deep", "x.txt")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := capability.RelTo(tc.root, tc.p); got != tc.want {
				t.Errorf("RelTo(%q, %q) = %q, want %q", tc.root, tc.p, got, tc.want)
			}
		})
	}

	// A path outside the root relates to it as an ascent, which the writer then
	// refuses — the point being that RelTo does not quietly pull it inside.
	outside := filepath.Join(t.TempDir(), "x.txt")
	got := capability.RelTo(root, outside)
	if !strings.HasPrefix(got, "..") {
		t.Errorf("RelTo of an out-of-tree path = %q, want an ascent the fence will refuse", got)
	}
	if err := capability.RepoWriterAt(binding(extension.WriteRepo), root).WriteFile(got, []byte("x"), 0o600); err == nil {
		t.Error("the converted path was accepted — RelTo must not launder a path into the tree")
	}
}

// A writer fenced at a root that does not exist must REFUSE, not create it. The
// fence is defined relative to a real tree; without one there is nothing to be
// inside of, and silently writing would put files wherever the path happened to
// point.
func TestAWriterWithNoRootRefuses(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-created")
	w := capability.RepoWriterAt(binding(extension.WriteRepo), missing)

	if err := w.WriteFile("x.txt", []byte("x"), 0o600); err == nil {
		t.Error("WriteFile succeeded with a non-existent fence root")
	}
	if err := w.PermitsWrite("x.txt"); err == nil {
		t.Error("PermitsWrite accepted a non-existent fence root")
	}
	if _, err := os.Stat(filepath.Join(missing, "x.txt")); err == nil {
		t.Error("the tree was created by a refused write")
	}
}
