package capability

// repowrite.go — the handle a `write-repo` binding receives.
//
// SIXTH CAPABILITY, and the last grant in the vocabulary with nothing behind it.
// `write-repo` has two holders — deliver-docs and environments — and until now it
// was the state `read-repo` was in before repo.go: a word on a declaration that
// no code consulted. RepoAt already honoured it (write implies read, as
// cluster-write implies cluster-read and secret-custody implies secret-read), so
// a write-repo binding could READ through a fence and then write with os.WriteFile
// past it.
//
// ────────────────────────────────────────────────────────────────────────────
// THE WRITE PATH HAS A HOLE THE READ PATH DOES NOT, and it is the whole reason
// this is a separate resolver rather than three more methods on repo.
//
// Repo.Resolve deliberately lets a NON-EXISTENT path through: EvalSymlinks fails
// on it, the lexical fence has already held, and refusing would mean a reader
// could not report "this file is missing" — a finding several guards exist to
// produce. For a read that is harmless; the open fails a moment later.
//
// For a WRITE it is an escape. Given a link `root/out -> /etc` the path
// `root/out/passwd` does not exist, so EvalSymlinks fails, so Resolve hands back
// the joined path unchecked — and os.WriteFile follows the link and writes
// /etc/passwd. The lexical check sees nothing wrong: no `..`, not absolute,
// textually inside the tree.
//
// So a write resolves its PARENT instead. The parent must exist for the write to
// succeed at all, so EvalSymlinks always has something to answer, and a link
// anywhere along the way lands outside the root and is refused. Same for MkdirAll
// and RemoveAll.
//
// THAT ALONE WAS NOT ENOUGH, AND THIS PARAGRAPH USED TO END HERE CLAIMING IT WAS.
// Resolving the parent closes a link one or more components ABOVE the target and
// says nothing about a link AT it: `root/out -> /etc/passwd` has the parent
// `root`, which is the tree itself. The write followed the leaf. A fence whose
// header asserts an escape is closed is worse than one that admits the gap,
// because the assertion is what the next reader checks instead of the code — so
// the leaf is resolved too now, and resolveForWrite says which check is which.
// ────────────────────────────────────────────────────────────────────────────
//
// ────────────────────────────────────────────────────────────────────────────
// ONE OF THE TWO HOLDERS IS NOT CONVERTED, AND IT CANNOT BE WITHOUT A DECISION
// NOBODY HAS MADE. Building this handle surfaced a conflict in the model itself.
//
// `environments` declares write-repo on ONE binding — `definition`, at
// `scaffolded` — and its extension header defends that split at length: folding
// it into the others "would widen the union so that reading the topology carried
// permission to write landingzone.yaml".
//
// But `llz spec set` and `llz env set` also write. They edit landingzone.yaml and
// environments/<env>.yaml through yamledit.EditSpecFile, and their bindings are
// `transition:configured` holding read-repo ALONE. So the declaration says those
// two commands only read, and they do not.
//
// It cannot be fixed by declaring the grant: validate.go pins
// `WriteRepo: {Scaffolded, Upgraded}`, so write-repo at `configured` is refused
// outright. The three ways out are (a) widen that row, (b) move the writes under
// the `definition` binding — undoing the split the header defends — or (c) decide
// `set` belongs at a different state. Each is a change to the vocabulary, and
// this repo's bar for that is two independent shipping cases plus an argument.
// This is case #1.
//
// So deliver-docs is converted and environments is not, deliberately. Wiring it
// by borrowing `definition`'s binding would have handed `set` a capability its
// own declaration refuses, which is the precise failure this layer exists to
// end — and it would have looked like progress.
// ────────────────────────────────────────────────────────────────────────────
//
// A REFUSED DESTRUCTIVE CALL MUST NEVER RETURN NIL, which is secrets.go's
// argument arriving in a new shape. `os.RemoveAll` returns nil for a path that
// was never there — success and no-op are the same answer — so a caller cannot
// tell "I deleted it" from "there was nothing". If a refusal ALSO returned nil,
// a denied binding's prune would look exactly like a completed one, and
// deliver-docs prunes a tree. Every refusal here is an error, and a test pins it.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// RepoWriter creates, rewrites and deletes files under one tree, and refuses
// everything outside it. It is separate from Repo because the grants are
// separate: every `write-repo` binding gets both, and a `read-repo` one gets a
// refusing writer.
type RepoWriter interface {
	// WriteFile writes data to a path relative to the tree root.
	WriteFile(rel string, data []byte, perm fs.FileMode) error
	// MkdirAll creates a directory and its parents, relative to the tree root.
	MkdirAll(rel string, perm fs.FileMode) error
	// RemoveAll deletes a path and everything under it. It reports a refusal as
	// an ERROR, never as the nil os.RemoveAll gives an absent path — see the
	// header.
	RemoveAll(rel string) error
	// PermitsWrite reports whether rel could be written, without writing it.
	PermitsWrite(rel string) error
}

// ErrNoRepoWrite is the refusal for a binding that did not declare write-repo.
// Distinct from ErrNoRepoRead for the reason ErrNoSecretCustody is distinct from
// ErrNoSecretRead: "I may not look" and "I may not change" are different bugs.
var ErrNoRepoWrite = errors.New("this binding does not declare write-repo, so it may not change the tree")

type repoWriter struct{ root string }

// resolveForWrite is the fence, and it differs from Repo.Resolve in exactly one
// way: it resolves the PARENT. See the header for the escape that closes.
func (w repoWriter) resolveForWrite(rel string) (string, error) {
	// repo(w) rather than repo{root: w.root}: the two structs are identical, and
	// the conversion says so instead of re-stating the field.
	if err := repo(w).Permits(rel); err != nil {
		return "", err
	}
	abs := filepath.Join(w.root, filepath.FromSlash(rel))

	rootReal, err := filepath.EvalSymlinks(w.root)
	if err != nil {
		return "", fmt.Errorf("resolving the repo root %q: %w", w.root, err)
	}
	// The target itself may not exist yet — that is the normal case for a create,
	// and MkdirAll may be several levels ahead of anything on disk. So the check
	// runs against the DEEPEST ANCESTOR THAT EXISTS: everything below it does not
	// exist, and a path that does not exist cannot be a symlink, so verifying the
	// existing part is sufficient.
	anchor, err := deepestExisting(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("resolving the parent of %q: %w", rel, err)
	}
	if !within(rootReal, anchor) {
		return "", fmt.Errorf("%w: %q resolves under %q, outside the tree",
			ErrOutsideRepo, rel, anchor)
	}
	// AND THE TARGET ITSELF, WHENEVER IT IS ALREADY A LINK. The anchor above
	// resolves the deepest existing ANCESTOR, which is what a create needs and
	// what a symlinked parent is caught by — but it stops one component short.
	//
	// The escape the header describes is `root/out -> /etc` plus the name
	// `passwd` that does not exist yet. ONE COMPONENT SHORTER IS THE SAME ESCAPE
	// AND WAS NOT CLOSED: with `root/out -> /etc/passwd`, the parent is `root`,
	// which resolves to itself inside the tree, and os.WriteFile follows the leaf
	// and overwrites the outside file. MkdirAll onto a link to an outside
	// DIRECTORY populates it; RemoveAll is the one operation that genuinely stops
	// at the link, and it is refused here anyway so the fence has one rule
	// instead of three.
	//
	// Repo.Resolve refuses this exact layout, so leaving it open made the READER
	// strictly stronger than the WRITER — the wrong direction for a fence whose
	// entire subject is mutation, and a claim this file's header made in the
	// opposite direction. TestTheWriteFenceIsNeverWeakerThanTheRead pins the
	// RELATION between the two resolvers rather than this one layout, because the
	// defect was the divergence and not the case that revealed it.
	//
	// Lstat, not Stat: Stat follows the link, so it answers about the target and
	// cannot see that there was a link at all.
	if fi, lerr := os.Lstat(abs); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			// A DANGLING LINK IS NOT A SAFE LINK, which is why this is a refusal
			// and not a fallthrough. `root/x -> /tmp/newfile` resolves to nothing
			// today and a write through it CREATES /tmp/newfile — the escape, just
			// with the outside file arriving a moment later. The anchor cannot see
			// it either: it walks up to `root`, which is inside the tree.
			return "", fmt.Errorf("%w: %q is a symlink that does not resolve (%v), so where a write to it would land cannot be established",
				ErrOutsideRepo, rel, err)
		}
		if !within(rootReal, real) {
			return "", fmt.Errorf("%w: %q is a symlink to %q, outside the tree",
				ErrOutsideRepo, rel, real)
		}
	}

	// The LEXICAL path is returned, not one rebuilt from the anchor. Rebuilding
	// dropped every segment between the anchor and the target — MkdirAll("a/b/c")
	// on an empty tree anchored at the root and created "c". The anchor's job is
	// to be CHECKED, not to be built from.
	return abs, nil
}

// deepestExisting walks up until a path resolves, so a create several directories
// deep can still be judged against a real ancestor.
func deepestExisting(p string) (string, error) {
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return real, nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", fs.ErrNotExist
		}
		p = parent
	}
}

func (w repoWriter) PermitsWrite(rel string) error {
	_, err := w.resolveForWrite(rel)
	return err
}

func (w repoWriter) WriteFile(rel string, data []byte, perm fs.FileMode) error {
	p, err := w.resolveForWrite(rel)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, perm)
}

func (w repoWriter) MkdirAll(rel string, perm fs.FileMode) error {
	p, err := w.resolveForWrite(rel)
	if err != nil {
		return err
	}
	return os.MkdirAll(p, perm)
}

func (w repoWriter) RemoveAll(rel string) error {
	p, err := w.resolveForWrite(rel)
	if err != nil {
		return err
	}
	return os.RemoveAll(p)
}

// deniedRepoWriter is what a binding without `write-repo` receives. Non-nil and
// refusing, like every other absent capability here.
type deniedRepoWriter struct{}

func (deniedRepoWriter) PermitsWrite(rel string) error {
	return fmt.Errorf("%w: %q", ErrNoRepoWrite, rel)
}
func (d deniedRepoWriter) WriteFile(rel string, _ []byte, _ fs.FileMode) error {
	return d.PermitsWrite(rel)
}
func (d deniedRepoWriter) MkdirAll(rel string, _ fs.FileMode) error { return d.PermitsWrite(rel) }

// RemoveAll REFUSES AS AN ERROR, NEVER AS NIL. os.RemoveAll answers a path that
// was never there with nil, so nil here would make a denied prune indistinguishable
// from a completed one — and deliver-docs prunes a tree. Same shape as
// deniedSecrets.Get returning Unknown rather than Absent.
func (d deniedRepoWriter) RemoveAll(rel string) error { return d.PermitsWrite(rel) }

// DeniedRepoWriter is the refusing handle, exported so a caller assembling its
// own Deps defaults to refusal rather than to nil.
func DeniedRepoWriter() RepoWriter { return deniedRepoWriter{} }

// RepoWriterAt builds the writer a binding's grants entitle it to, fenced at root.
//
// Only `write-repo` produces one. `read-repo` does NOT imply it — that is the
// direction the implication runs, and the whole point of having two grants.
func RepoWriterAt(b extension.Binding, root string) RepoWriter {
	for _, g := range b.Grants {
		if g == extension.WriteRepo {
			return repoWriter{root: root}
		}
	}
	return deniedRepoWriter{}
}

// RepoContainingWriter is RepoContaining's writing half: a writer fenced to the
// tree that CONTAINS the given path, plus that path re-expressed under it.
//
// It shares RepoContainingAll's ascent rule rather than reimplementing it, so a
// writer and a reader handed the same path are always fenced identically — two
// path rules that agree by accident is the shape this repo keeps paying for.
func RepoContainingWriter(b extension.Binding, path string) (RepoWriter, string) {
	r, rels := RepoContainingAll(b, []string{path})
	if _, ok := r.(repo); !ok {
		// The binding produced a refusing reader, so it must produce a refusing
		// writer too — not one fenced at a root it was never granted.
		return deniedRepoWriter{}, rels[0]
	}
	return RepoWriterAt(b, r.(repo).root), rels[0]
}

// RelTo converts a path a caller already holds into one relative to a fence root,
// which is what every converted writer needs and what two of them got subtly
// wrong before it lived here.
//
// IT IS NOT filepath.Rel. Two things break that:
//
//   - MIXED SPELLINGS. Flags that name trees are independent of each other, and
//     callers mix absolute and relative freely — deliver-docs takes `--docs` and
//     `--root` separately, and render's tests pass an absolute tree where
//     production passes a relative one. filepath.Rel refuses to relate the two
//     forms and returns an error, which reads as "unrelatable" when it means
//     "spelled differently".
//   - THE macOS ALIAS. The working directory is reached through /var while an
//     absolute path under it resolves to /private/var, so relating one to the
//     other yields a ../.. chain out of the tree — and the fence then correctly
//     refuses a write that was always legitimate.
//
// So both sides are resolved to one form first, and resolved the SAME way whether
// or not they exist yet. Resolving only the parent was the first attempt and it
// failed on exactly the case render needs: the target's directory is about to be
// created by MkdirAll, so EvalSymlinks cannot answer for it, one side stayed /var
// while the other became /private/var, and every write was refused. So the
// deepest EXISTING ancestor is resolved and the not-yet-created remainder is
// re-appended — a path that does not exist cannot be a symlink, so nothing is
// lost by carrying it verbatim.
//
// A path that genuinely cannot be related is returned UNCHANGED, so the writer
// refuses it loudly. Silently retargeting it somewhere inside the tree would be
// the worse failure — a write that succeeds against the wrong file.
func RelTo(root, p string) string {
	rel, err := filepath.Rel(resolveEvenIfAbsent(root), resolveEvenIfAbsent(p))
	if err != nil {
		return p
	}
	return rel
}

// resolveEvenIfAbsent returns x absolute and symlink-resolved as far as it can
// be, carrying any not-yet-created tail verbatim.
func resolveEvenIfAbsent(x string) string {
	abs, err := filepath.Abs(x)
	if err != nil {
		return x
	}
	anc, tail := abs, ""
	for {
		if real, err := filepath.EvalSymlinks(anc); err == nil {
			return filepath.Join(real, tail)
		}
		parent := filepath.Dir(anc)
		if parent == anc {
			return abs
		}
		tail = filepath.Join(filepath.Base(anc), tail)
		anc = parent
	}
}
