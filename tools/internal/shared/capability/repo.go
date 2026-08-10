package capability

// repo.go — the handle for `read-repo`, whose fence is PATH CONTAINMENT.
//
// The counts here are pinned by TestHandleHeaderCensusesMatchTheRegistry — they
// were hand-transcribed once and had drifted by the time anyone re-read them.
//
// FIFTH CAPABILITY, AND THE LAST GRANT WITHOUT ONE. `read-repo` is declared by 43
// of 63 extensions — more than any other grant — and until now it meant nothing at
// runtime. The validator refuses a gate that declares anything ELSE
// (checkBindingCeiling), so the entire safety claim of `llz ci gates` — "these
// touch nothing but files" — rested on a check of the DECLARATION and on nothing
// stopping a gate from reading ~/.aws/credentials.
//
// ────────────────────────────────────────────────────────────────────────────
// IT IS ONE GRANT, NOT THREE, AND THAT WAS MEASURED RATHER THAN ASSUMED.
//
// An earlier architecture critique proposed splitting `read-repo` into reading the
// SPEC, the TREE, and `.template-manifest`'s class table, arguing they are
// materially different permissions. Counted across the holders at the time (40 of
// the then-61 extensions; the shape is what matters, not the total):
//
//	tree      26    os.ReadFile / filepath.Walk / guardwalk
//	manifest  12    .template-manifest, .copier-answers.yml
//	spec       7    clusterspec.Load* / Detected
//	none       6    hold the grant, read none of the three
//
// The categories OVERLAP AND DO NOT PARTITION: `render` reads all three; six other
// packages read two. A split would have most bindings declaring two or three
// grants where they declare one — noisier without being more informative, which is
// the same argument that made secret-custody imply secret-read.
//
// So: one capability, one fence.
// ────────────────────────────────────────────────────────────────────────────
//
// WHY THIS IS HARDER THAN THE OTHER FOUR, and why nothing is converted yet.
// Cluster, Secrets, Forge and BaoAdmin each intercepted a SEAM — a package-level
// func var callers already went through, so converting meant changing what the
// seam resolved to. `read-repo` has no seam: its callers use os.ReadFile directly,
// 124 times across 40 packages. There is nothing to intercept.
//
// That makes a half-converted tree the dangerous state — one where the fence
// exists and does not hold, which reads as protection and is not. The handle and
// its guarantees land first (as with Forge and BaoAdmin); the guards/ bucket is
// the first customer, because a gate is what runs in a pre-commit hook on a
// laptop and guardkit already answers half the question for those fifteen.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// Repo reads files under one tree, and refuses everything outside it.
type Repo interface {
	// ReadFile reads a path relative to the tree root.
	ReadFile(rel string) ([]byte, error)
	// Stat reports on a path relative to the tree root.
	Stat(rel string) (os.FileInfo, error)
	// ReadDir lists one directory, as os.ReadDir does. Present because the census
	// found twelve os.ReadDir sites among the `read-repo` holders — the method set
	// here is what the callers actually use, not a filesystem abstraction someone
	// designed.
	ReadDir(rel string) ([]os.DirEntry, error)
	// WalkDir walks a subtree, refusing to descend outside the root.
	WalkDir(rel string, fn fs.WalkDirFunc) error
	// Resolve returns the absolute on-disk path for rel, or an error if rel
	// escapes. Exported for callers that must hand a path to something else — a
	// shell-out, an embed, a library that takes a filename.
	Resolve(rel string) (string, error)
	// Permits reports whether rel is inside the tree, without touching the disk.
	Permits(rel string) error
}

type repo struct{ root string }

// ErrOutsideRepo is the refusal. One value, so a caller can tell a fence
// violation from a missing file — they are different bugs, and conflating them is
// how a fence gets "fixed" by creating the file.
var ErrOutsideRepo = errors.New("path escapes the repository tree")

// ErrNoRepoRead is the refusal for a binding that declared no repo access.
var ErrNoRepoRead = errors.New("this binding does not declare read-repo, so it may not read the tree")

// Permits is the fence, and it is deliberately three checks rather than one.
//
// AN ABSOLUTE PATH IS REFUSED OUTRIGHT. filepath.Join(root, "/etc/passwd") yields
// root/etc/passwd — it does NOT escape, so a naive containment check passes and
// the caller silently reads the wrong file. The bug is the caller believing an
// absolute path means what it says, so the honest answer is to refuse it.
//
// `..` IS REFUSED AFTER CLEANING, not before. "a/../../etc" contains no leading
// `..` and escapes anyway; filepath.Clean is what makes the check meaningful.
//
// SYMLINKS ARE CHECKED AT RESOLVE TIME, not here. A link inside the tree pointing
// outside it passes every lexical test — and this repo's own trees contain links
// (the vendored workflow bodies). Permits stays pure and cheap so a caller can ask
// before doing work; Resolve does the syscall.
func (r repo) Permits(rel string) error {
	if filepath.IsAbs(rel) {
		return fmt.Errorf("%w: %q is absolute — a path relative to the tree was expected, "+
			"and joining an absolute path silently reads a different file", ErrOutsideRepo, rel)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %q resolves above the root", ErrOutsideRepo, rel)
	}
	return nil
}

// Resolve applies the fence and then verifies the RESULT is still inside the
// tree after symlinks are followed. That second half is the one a lexical check
// cannot do: a link inside the tree pointing at /etc is invisible to Clean.
//
// A path that does not exist yet is allowed through — EvalSymlinks fails on it,
// and refusing would mean a reader could not report "this file is missing", which
// is a finding several guards exist to produce.
func (r repo) Resolve(rel string) (string, error) {
	if err := r.Permits(rel); err != nil {
		return "", err
	}
	abs := filepath.Join(r.root, filepath.FromSlash(rel))
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs, nil // does not exist yet; the lexical fence already held
	}
	rootReal, err := filepath.EvalSymlinks(r.root)
	if err != nil {
		return "", fmt.Errorf("resolving the repo root %q: %w", r.root, err)
	}
	if !within(rootReal, real) {
		return "", fmt.Errorf("%w: %q is a symlink to %q, outside the tree",
			ErrOutsideRepo, rel, real)
	}
	return real, nil
}

func (r repo) ReadFile(rel string) ([]byte, error) {
	p, err := r.Resolve(rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func (r repo) Stat(rel string) (os.FileInfo, error) {
	p, err := r.Resolve(rel)
	if err != nil {
		return nil, err
	}
	return os.Stat(p)
}

// ReadDir lists a directory, and REFUSES THE ENTRIES THAT LEAVE THE TREE rather
// than the directory that contains one. A tree holding a single outward symlink
// is still a tree worth listing — the same call the walk makes, for the same
// reason — and the caller's next move with any entry is a read, which the fence
// would refuse anyway. Dropping it here makes that refusal happen once, in the
// place that can explain it.
func (r repo) ReadDir(rel string) ([]os.DirEntry, error) {
	p, err := r.Resolve(rel)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	rootReal, err := filepath.EvalSymlinks(r.root)
	if err != nil {
		return nil, fmt.Errorf("resolving the repo root %q: %w", r.root, err)
	}
	kept := entries[:0]
	for _, e := range entries {
		if real, lerr := filepath.EvalSymlinks(filepath.Join(p, e.Name())); lerr == nil && !within(rootReal, real) {
			continue
		}
		kept = append(kept, e)
	}
	return kept, nil
}

// WalkDir refuses to hand the callback anything outside the tree. A walk is where
// a symlink escape actually bites: the root is legitimate, and one entry inside it
// points elsewhere.
//
// IT HANDS THE CALLBACK TREE-RELATIVE PATHS, NOT ON-DISK ONES, and that is the
// property that makes the walk usable rather than merely safe. Every caller in
// this tree does the same thing with the path it is given — reads it, or prints
// it as a finding — and both of those go back through this handle, whose other
// methods take a tree-relative path and REFUSE an absolute one. A walk yielding
// resolved paths would therefore produce paths its own ReadFile rejects: the
// fence would hold and the reader would be unusable. Caught the first time
// guardwalk was pointed at a real corpus.
func (r repo) WalkDir(rel string, fn fs.WalkDirFunc) error {
	start, err := r.Resolve(rel)
	if err != nil {
		return err
	}
	rootReal, err := filepath.EvalSymlinks(r.root)
	if err != nil {
		return fmt.Errorf("resolving the repo root %q: %w", r.root, err)
	}
	// The walk starts at the RESOLVED path, so the prefix to strip is that path
	// and not r.root — they differ whenever the root or any parent of it is a
	// symlink, which is the normal case under macOS's /var and every t.TempDir().
	base := filepath.Clean(filepath.FromSlash(rel))
	return filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fn(unresolve(base, start, p), d, err)
		}
		if real, lerr := filepath.EvalSymlinks(p); lerr == nil && !within(rootReal, real) {
			// Skip the escape rather than failing the walk: a guard scanning a tree
			// that happens to contain one link outward should report on the rest,
			// not abort. What it must not do is READ through it.
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		return fn(unresolve(base, start, p), d, nil)
	})
}

// unresolve turns an on-disk path produced by the walk back into one relative to
// the tree, expressed under the same `rel` the caller asked to walk — so a guard
// told to scan "platform-apl" reports "platform-apl/x/y.yaml" and can read it
// back through the same handle.
func unresolve(base, start, p string) string {
	rest, err := filepath.Rel(start, p)
	if err != nil {
		return p
	}
	return filepath.Join(base, rest)
}

// within reports whether p is root or beneath it. Compares path SEGMENTS, not
// strings: a prefix test would accept "/repo-backup" as inside "/repo".
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

type deniedRepo struct{}

func (deniedRepo) Permits(rel string) error {
	return fmt.Errorf("%w: %q", ErrNoRepoRead, rel)
}
func (d deniedRepo) Resolve(rel string) (string, error)         { return "", d.Permits(rel) }
func (d deniedRepo) ReadFile(rel string) ([]byte, error)        { return nil, d.Permits(rel) }
func (d deniedRepo) Stat(rel string) (os.FileInfo, error)       { return nil, d.Permits(rel) }
func (d deniedRepo) ReadDir(rel string) ([]os.DirEntry, error)  { return nil, d.Permits(rel) }
func (d deniedRepo) WalkDir(rel string, _ fs.WalkDirFunc) error { return d.Permits(rel) }

// DeniedRepo is the refusing handle, exported so a caller assembling its own Deps
// defaults to refusal rather than to nil.
func DeniedRepo() Repo { return deniedRepo{} }

// RepoAt builds a reader fenced to root, for a binding that declares read-repo.
// The ROOT comes from the caller because only it knows which tree it was pointed
// at — guards take `--root`, and an instance and the template repo are different
// trees with the same shape.
func RepoAt(b extension.Binding, root string) Repo {
	for _, g := range b.Grants {
		if g == extension.ReadRepo || g == extension.WriteRepo {
			return repo{root: root}
		}
	}
	return deniedRepo{}
}

// RepoContaining is for the callers whose flag names a TREE TO SCAN rather than a
// repository root — `--render-dir rendered`, `--dir ../.github/workflows`. It
// returns a reader fenced to the tree that CONTAINS that directory, plus the
// directory re-expressed under it, so a finding still names the file the way the
// caller asked for it.
//
// Fencing at the scan directory itself would be tighter and is wrong: the paths a
// guard prints would lose the segment the operator typed, so `rendered/x.yaml`
// would come back as `x.yaml` and two scan roots would produce colliding names.
// The containing tree is the smallest fence that keeps the findings meaningful.
func RepoContaining(b extension.Binding, dir string) (Repo, string) {
	r, dirs := RepoContainingAll(b, []string{dir})
	return r, dirs[0]
}

// RepoContainingAll is RepoContaining for a caller whose flag is a LIST of trees
// — monitoring-label-guard's `--root`, which is a set of directories to scan and
// not a repository root like its thirteen siblings'. All of them must share one
// reader, or their findings collide: `platform-apl/x.yaml` and `rendered/x.yaml`
// would both come back as `x.yaml`.
//
// ────────────────────────────────────────────────────────────────────────────
// THE ROOT IS THE ASCENT, and getting this wrong is what `llz ci gates` caught.
//
// The driver invokes these two from tools/ as `--root ..` and
// `--dir ../.github/workflows`. A first cut fenced a relative scan dir at the
// working directory, reasoning that a relative path is "already under" it — which
// is true of `rendered` and false of anything beginning `..`. Both gates refused
// their own corpus on the first real run.
//
// So the fence goes at the LEADING `..` RUN, and everything below it is the path
// the caller keeps: `../.github/workflows` fences at `..` and scans
// `.github/workflows`, which is both correct and the shape whose findings a
// reviewer can open. A scan dir with no ascent fences at the working directory,
// as before.
// ────────────────────────────────────────────────────────────────────────────
func RepoContainingAll(b extension.Binding, dirs []string) (Repo, []string) {
	if len(dirs) == 0 {
		return RepoAt(b, "."), nil
	}
	// An ABSOLUTE dir has no ascent to find; its containing tree is its parent.
	// Mixed absolute and relative is not a shape any caller has, and guessing a
	// common root across the two would be inventing a rule off zero cases.
	if filepath.IsAbs(filepath.Clean(filepath.FromSlash(dirs[0]))) {
		clean := filepath.Clean(filepath.FromSlash(dirs[0]))
		return RepoAt(b, filepath.Dir(clean)), []string{filepath.Base(clean)}
	}
	// The fence must be high enough for EVERY dir, so it is the longest ascent
	// among them.
	root := "."
	for _, d := range dirs {
		if a := ascentOf(d); len(a) > len(root) {
			root = a
		}
	}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		rel, err := filepath.Rel(root, filepath.Clean(filepath.FromSlash(d)))
		if err != nil {
			// Unrelatable to the chosen root: hand it over untouched so the fence
			// refuses it loudly, rather than silently scanning somewhere else.
			rel = d
		}
		out = append(out, rel)
	}
	return RepoAt(b, root), out
}

// ascentOf returns the leading run of ".." segments in a relative path, or "."
// when it has none. That run is how far above the working directory the path
// reaches, and therefore where a reader has to be fenced for it to be readable at
// all.
func ascentOf(p string) string {
	segs := strings.Split(filepath.ToSlash(filepath.Clean(filepath.FromSlash(p))), "/")
	n := 0
	for n < len(segs) && segs[n] == ".." {
		n++
	}
	if n == 0 {
		return "."
	}
	return filepath.Join(segs[:n]...)
}

// RepoForGate is RepoAt for the gate binding an extension declares.
//
// It exists so the fifteen guards do not each hand-roll the binding lookup, and
// so the reader provably comes FROM the declaration rather than from a binding
// the guard invented next to itself — which is the entire claim the grant
// vocabulary makes. `capability.For` is not usable here for the same reason
// `seedBinding()` exists in the lifecycle packages: the handles belong to a
// BINDING, and an extension with several would otherwise hand back the union.
//
// It PANICS rather than returning a refusing reader when there is no gate
// binding. A guard whose declaration lost its gate is a wiring bug, and the
// refusing reader would express it as every file being unreadable — which reads
// as a broken checkout, not as a missing declaration.
func RepoForGate(e extension.Extension, root string) Repo {
	for _, b := range e.Bindings {
		if b.Kind == extension.Gate {
			return RepoAt(b, root)
		}
	}
	panic(fmt.Sprintf("%s: no gate binding — its guard builds its read-repo reader from one, "+
		"so its absence is a wiring bug rather than a missing feature", e.Name))
}
