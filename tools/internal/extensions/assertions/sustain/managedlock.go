package sustain

// ci_managed_lock.go implements `llz ci managed-fresh` — the drift guard specified
// in docs/designs/cross-org-reuse-pattern.md: "a hand-edited instance graph fails
// CI rather than silently diverging".
//
// WHY A HASH LOCK AND NOT A RE-RENDER: the obvious check is "render the template
// at the pinned ref and diff", but `copier` ships only in the devcontainer image —
// the reusable workflows run in ci-tofu (vars.TF_IMAGE), which has no copier
// and, on an air-gapped GHE, no route to the template repo either (ADR 0003). So
// the template instead SHIPS the expected digests in .template-managed.lock and
// the guard recomputes them locally: no network, no Python, no template checkout.
//
// WHY THAT IS SOUND: a `managed` file that carries no `<@ … @>` copier
// substitution renders byte-identically in every instance, so one digest covers
// them all. Tokenful `managed` files render per-instance and cannot be locked at
// all; they are recorded in the lock header as declared exclusions so the gap is
// visible rather than silently absent.
//
// SCOPE IS THE MANIFEST, NOT A PREFIX: this guard used to cover only `.github/`,
// which left 16 of the 31 lockable `managed` files (.tflintrc.hcl, .checkov.yaml,
// .gitleaks.toml, apl-values/_shared/apl-overlay/appvalues.yaml, …) overwritten by
// `llz upgrade` with no
// drift warning at all — the exact failure this exists to prevent, on more than
// half the surface. The lock is now a projection of .template-manifest's
// digestLocked classes, so the two cannot drift apart.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// ManagedLockPath is the digest list, relative to the scaffold root. It is
// itself `managed`, so `llz upgrade` refreshes it alongside the files it covers.
// Named for the CLASS it locks, not a directory: it used to be
// .template-workflows.lock, scoped to .github/, and the name outlived the scope.
const ManagedLockPath = ".template-managed.lock"

// ManagedDrift is the template-owned files whose bytes no longer match the
// digests the template shipped: edited locally, or deleted.
//
// SPLIT OUT SO `llz upgrade` CAN ASK THE SAME QUESTION. The upgrade overwrites
// every `managed` file from a clean render, so a local edit to one is discarded —
// and the only moment that edit is still observable is BEFORE copier runs. Having
// the upgrade recompute "does this file match the lock" would be a second copy of
// the rule this file defines, which is the split-contract shape docs/e2e-gates.md
// warns about: two implementations of one digest comparison, each with its own
// idea of which classes are locked.
type ManagedDrift struct {
	Edited  []string // present, and its bytes differ from the lock
	Missing []string // recorded in the lock, absent from the tree
}

// Any reports whether anything drifted.
func (m ManagedDrift) Any() bool { return len(m.Edited)+len(m.Missing) > 0 }

// All is every drifted path, missing first — the order the guard reports them in.
func (m ManagedDrift) All() []string {
	return append(append([]string{}, m.Missing...), m.Edited...)
}

// DriftedManaged compares an instance's digest-locked files against the lock it
// carries, and also returns the lock so a caller that needs its size does not
// read it twice.
//
// A MISSING LOCK IS NOT DRIFT. An instance rendered before the lock existed has
// nothing to compare against; it reports clean with a nil lock, and both callers
// treat that as "nothing to say" rather than as a clean bill of health.
func DriftedManaged(d Deps, root string) (ManagedDrift, map[string]string, error) {
	// AN UNWIRED SEAM IS AN ERROR, NOT A PANIC. Deps is assembled caller-side and
	// LockableScaffoldFiles is optional in practice — the upgrade package's own
	// TestMain leaves it nil, on the then-true reasoning that nothing reachable
	// needed it. A new advisory caller made it reachable, and a bare call turned
	// "this deps set cannot answer" into a crash in `llz upgrade`. That is the
	// shape a seam defaulting to "package main assigns it" always fails in; an
	// error lets the advisory degrade to silence while the guard, which cannot
	// work without it, still fails loudly.
	if d.LockableScaffoldFiles == nil {
		return ManagedDrift{}, nil, fmt.Errorf("managed-drift: no LockableScaffoldFiles in Deps — " +
			"the caller assembled a Deps that cannot resolve the scaffold")
	}
	scaffoldRoot, _, err := d.LockableScaffoldFiles(root)
	if err != nil {
		return ManagedDrift{}, nil, err
	}
	want, err := ReadManagedLock(managedLockPathFor(scaffoldRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return ManagedDrift{}, nil, nil
		}
		return ManagedDrift{}, nil, err
	}
	return compareManagedLock(scaffoldRoot, want), want, nil
}

// compareManagedLock is the digest comparison itself — the one loop both the
// guard and `llz upgrade` reach, so neither can grow its own opinion of what
// "matches the template" means.
//
// AN UNREADABLE FILE COUNTS AS MISSING, not as a match. sha256File fails the same
// way for a deleted file and an unreadable one, and treating either as clean
// would report the strongest verdict on the weakest evidence.
func compareManagedLock(scaffoldRoot string, want map[string]string) ManagedDrift {
	var drift ManagedDrift
	for _, rel := range sortedKeys(want) {
		sum, err := sha256File(filepath.Join(scaffoldRoot, filepath.FromSlash(rel)))
		if err != nil {
			drift.Missing = append(drift.Missing, rel)
			continue
		}
		if sum != want[rel] {
			drift.Edited = append(drift.Edited, rel)
		}
	}
	return drift
}

// managedLockPathFor keeps the one join-or-not rule in a single place; a
// scaffoldRoot of "." must not produce "./.template-managed.lock".
func managedLockPathFor(scaffoldRoot string) string {
	if scaffoldRoot == "." {
		return ManagedLockPath
	}
	return filepath.Join(scaffoldRoot, ManagedLockPath)
}

func RunManagedFresh(d Deps, root string, write bool, out, errOut io.Writer) error {
	scaffoldRoot, lockable, err := d.LockableScaffoldFiles(root)
	if err != nil {
		return err
	}
	lockPath := managedLockPathFor(scaffoldRoot)

	if write {
		return writeManagedLock(scaffoldRoot, lockable, lockPath, out)
	}

	want, err := ReadManagedLock(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Instances rendered before the lock existed simply have nothing to
			// check — don't fail their lint gate; the next `llz upgrade` ships one.
			fmt.Fprintf(errOut, "managed-fresh: no %s — skipping (upgrade to a template version that ships it)\n", ManagedLockPath)
			return nil
		}
		return err
	}

	drift := compareManagedLock(scaffoldRoot, want)
	drifted, missing := drift.Edited, drift.Missing
	if !drift.Any() {
		if err := checkLockComplete(scaffoldRoot, lockable, want, errOut); err != nil {
			return err
		}
		fmt.Fprintf(out, "managed-fresh: OK — %d template-owned file(s) match %s\n", len(want), ManagedLockPath)
		return nil
	}

	for _, rel := range missing {
		fmt.Fprintf(errOut, "::error file=%s::template-owned file is missing\n", rel)
	}
	for _, rel := range drifted {
		fmt.Fprintf(errOut, "::error file=%s::file was edited locally — the template owns it and `llz upgrade` will overwrite it\n", rel)
	}
	fmt.Fprintf(errOut, "\n%s %d template-owned file(s) drifted from the template:\n", color.Red("✗"), len(drifted)+len(missing))
	for _, rel := range append(append([]string{}, missing...), drifted...) {
		fmt.Fprintf(errOut, "    %s\n", rel)
	}
	fmt.Fprintf(errOut, "\nThese are in a digest-locked class in .template-manifest — the template owns\n"+
		"them and the next `llz upgrade` overwrites them from a clean render, so a local\n"+
		"edit here is lost.\n"+
		"Fix by either:\n"+
		"  • reverting the edit (`llz upgrade` re-syncs them), or\n"+
		"  • sending the change upstream to the template, where it belongs.\n")
	return fmt.Errorf("managed-fresh: %d template-owned file(s) drifted", len(drifted)+len(missing))
}

// lockableFiles splits the scaffold into what the digest lock can cover and what
// it deliberately cannot. A file is lockable when its class is digestLocked (the
// manifest is the authority — there is no path prefix here) and it carries no
// copier token, since a token makes the rendered bytes per-instance.
//
// The lock file itself is excluded by construction: it is `managed`, so it would
// otherwise try to record a digest of the bytes being written.
func lockableFiles(scaffoldRoot string, files []string) (sums map[string]string, tokenful []string, err error) {
	sums = map[string]string{}
	for _, rel := range files {
		if rel == ManagedLockPath {
			continue
		}
		data, err := capability.RepoAt(readBinding(), scaffoldRoot).ReadFile(filepath.FromSlash(rel))
		if err != nil {
			return nil, nil, fmt.Errorf("managed-fresh: read %s: %w", rel, err)
		}
		if strings.Contains(string(data), "<@") {
			tokenful = append(tokenful, rel)
			continue
		}
		sum := sha256.Sum256(data)
		sums[rel] = hex.EncodeToString(sum[:])
	}
	sort.Strings(tokenful)
	return sums, tokenful, nil
}

// checkLockComplete fails when a file the lock COULD cover is absent from it —
// the "someone added a managed file and forgot to regenerate" hole. Without it
// the verify pass only walks the lock's own keys, so an unlocked managed file is
// indistinguishable from one that does not exist, and the lock silently decays
// into a subset of the manifest instead of a projection of it.
//
// Template-repo only (gated on copier.yml next to the scaffold root, the same
// guard checkCopierFencing uses): an instance is rendered output, and holding it
// to the template's completeness invariant would fail instances whose lock simply
// predates a newly-classified file.
func checkLockComplete(scaffoldRoot string, lockable []string, want map[string]string, errOut io.Writer) error {
	if !fileExists(filepath.Join(filepath.Dir(scaffoldRoot), "copier.yml")) &&
		!fileExists(filepath.Join(filepath.Dir(scaffoldRoot), "copier.yaml")) {
		return nil
	}
	sums, _, err := lockableFiles(scaffoldRoot, lockable)
	if err != nil {
		return err
	}
	var unlocked []string
	for _, rel := range sortedKeys(sums) {
		if _, ok := want[rel]; !ok {
			unlocked = append(unlocked, rel)
		}
	}
	if len(unlocked) == 0 {
		return nil
	}
	for _, rel := range unlocked {
		fmt.Fprintf(errOut, "::error file=%s::template-owned file is missing from %s\n", rel, ManagedLockPath)
	}
	fmt.Fprintf(errOut, "\n%s %d template-owned file(s) are not covered by %s:\n", color.Red("✗"), len(unlocked), ManagedLockPath)
	for _, rel := range unlocked {
		fmt.Fprintf(errOut, "    %s\n", rel)
	}
	fmt.Fprintf(errOut, "\n`llz upgrade` overwrites these from a clean render, so an instance's local edit\n"+
		"is silently lost — which is exactly what this lock exists to catch. Regenerate it:\n"+
		"    llz ci managed-fresh --root %s --write\n", scaffoldRoot)
	return fmt.Errorf("managed-fresh: %d template-owned file(s) missing from the lock", len(unlocked))
}

// writeManagedLock regenerates the digest list from the scaffold, covering every
// token-free file in a digestLocked class. Tokenful ones cannot be locked at all
// (their rendered bytes differ per instance) and are recorded in the header as
// declared exclusions rather than dropped silently.
func writeManagedLock(scaffoldRoot string, lockable []string, lockPath string, out io.Writer) error {
	sums, tokenful, err := lockableFiles(scaffoldRoot, lockable)
	if err != nil {
		return err
	}
	if len(sums) == 0 {
		return fmt.Errorf("managed-fresh: no digest-lockable files in %s — refusing to write an empty lock", scaffoldRoot)
	}

	var b strings.Builder
	b.WriteString("# " + ManagedLockPath + " — digests of the template-owned scaffold surface.\n")
	b.WriteString("#\n")
	b.WriteString("# GENERATED by `llz ci managed-fresh --write` — do not hand-edit.\n")
	b.WriteString("# Covers every token-free file in a digest-locked class of .template-manifest\n")
	b.WriteString("# (today: `managed`). Those render byte-identically in every instance, which is\n")
	b.WriteString("# what makes one digest valid for all of them.\n")
	b.WriteString("#\n")
	b.WriteString("# `llz ci managed-fresh` (part of `llz lint`) recomputes these offline and fails\n")
	b.WriteString("# when an instance hand-edits a file the next `llz upgrade` would overwrite.\n")
	if len(tokenful) > 0 {
		b.WriteString("#\n")
		b.WriteString("# DELIBERATELY UNLOCKED — these are digest-locked-class files that carry a copier\n")
		b.WriteString("# token, so their rendered bytes differ per instance and no single digest can\n")
		b.WriteString("# cover them. `llz upgrade` still overwrites them from a clean render (which is\n")
		b.WriteString("# correct — the render substitutes each instance's own tokens); they simply get\n")
		b.WriteString("# no drift detection. Listed so the gap is auditable rather than invisible:\n")
		for _, rel := range tokenful {
			b.WriteString("#   " + rel + "\n")
		}
	}
	b.WriteString("#\n")
	b.WriteString("# FORMAT: <sha256>  <path>\n")
	for _, rel := range sortedKeys(sums) {
		fmt.Fprintf(&b, "%s  %s\n", sums[rel], rel)
	}
	// THE ONE WRITE, and through the ONE binding that declares write-repo. Using
	// readBinding() here would hand the managed-fresh check the power to rewrite
	// the lock it verifies.
	wrepo, wrel := capability.RepoContainingWriter(writeBinding(), lockPath)
	if err := wrepo.WriteFile(wrel, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("managed-fresh: write %s: %w", lockPath, err)
	}
	fmt.Fprintf(out, "managed-fresh: wrote %s (%d file(s))\n", lockPath, len(sums))
	return nil
}

// ReadManagedLock reads the digest list through the fence. It takes a PATH
// rather than a root because both callers already have one built, and its tests
// pass an absolute temp file — RepoContaining accepts either.
func ReadManagedLock(path string) (map[string]string, error) {
	repo, rel := capability.RepoContaining(readBinding(), path)
	// ReadFile rather than os.Open: the reader deliberately exposes no Open, so
	// that every path into the tree is one the fence has judged. The lock is a
	// digest list — a few hundred lines — so holding it in memory costs nothing.
	raw, err := repo.ReadFile(rel)
	if err != nil {
		return nil, err
	}
	sums := map[string]string{}
	s := bufio.NewScanner(bytes.NewReader(raw))
	lineNo := 0
	for s.Scan() {
		lineNo++
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 || len(parts[0]) != 64 {
			return nil, fmt.Errorf("managed-fresh: %s:%d bad entry (expected `<sha256>  <path>`): %q", path, lineNo, line)
		}
		sums[filepath.ToSlash(parts[1])] = parts[0]
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("managed-fresh: read %s: %w", path, err)
	}
	return sums, nil
}

// sha256File hashes one file THROUGH THE FENCE. Its caller joins scaffoldRoot on
// already, so the path arrives absolute-or-relative depending on the caller's
// spelling — RepoContaining relates the two rather than refusing one.
func sha256File(path string) (string, error) {
	repo, rel := capability.RepoContaining(readBinding(), path)
	data, err := repo.ReadFile(rel)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// ── localised pure helpers: copies, not seams ──────────────────────────────

// sortedKeys returns a map's keys in sorted order, so output is stable.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fileExists reports whether path exists. Pure, localised — package main keeps
// its copy for the manifest machinery ADR 0014 pins as core.
func fileExists(path string) bool {
	repo, rel := capability.RepoContaining(readBinding(), path)
	_, err := repo.Stat(rel)
	return err == nil
}
