package upgrade

// convergence.go — the assertion `llz ci upgrade-test` was missing: an instance
// that UPGRADES to a ref must end up holding the same bytes as one SCAFFOLDED at
// that ref.
//
// WHY THIS AND NOT MORE CHECKS OF THE OLD SHAPE. The four checks the gate already
// makes (the update ran unattended, the answers survived, the pin moved, no merge
// artifacts) all ask whether the update MECHANISM behaved. None of them asks
// whether the instance ended up CORRECT. Between those two questions sits the
// entire delivery half of an upgrade — every file the template changed, added or
// dropped in the releases being skipped — and a gate that never compares against a
// known-good tree cannot see any of it. The known-good tree is free: it is a fresh
// scaffold at the same ref, which `instance-test` already proves is well-formed.
//
// IT FOUND ONE IMMEDIATELY. `llz upgrade` is three steps — `copier update`, the
// manifest-class policy pass, then the declared removals — and the policy pass
// sourced its `managed` files from a render built with `--skip-tasks`. The docs
// task is what repoints AGENTS.md's `docs/adopter-guide.md` link at the template
// repo (adopter-guide.md is pruned out of an instance), so every upgraded instance
// had that link reverted to a dead relative path while every fresh one was
// correct. Both halves had unit tests. Neither had a test of the two composed.
//
// THE COMPARISON IS CLASS-AWARE, and has to be. .template-manifest already says
// what an upgrade is supposed to do per file, so this asks the manifest rather
// than restating it:
//
//	managed  overwrite → bytes MUST match the fresh scaffold.
//	merge    3-way     → bytes must match too, and this is not a weaker claim: with
//	                     no instance-local edits, `ours` equals the merge base, so a
//	                     correct 3-way merge yields `theirs` exactly. A difference
//	                     here is a real merge defect, not noise.
//	owned    restore   → EXCLUDED. Seeded once and never updated by design, so an
//	                     instance scaffolded at an older release legitimately keeps
//	                     that release's copy. Asserting equality here would make the
//	                     gate go red the first time a provider lockfile changed —
//	                     a false alarm that gets a gate switched off.
//
// Reported, never inferred: the class of every divergence is printed, so a reader
// learns which of the three mechanisms broke instead of which file happened to
// differ.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/manifest"
)

// ConvergenceGap is one file on which the upgraded instance and a fresh scaffold
// at the same ref disagree.
type ConvergenceGap struct {
	Path  string
	Class string // .template-manifest class, or "" when the file matches no rule
	Kind  string // missing | stale | orphaned
}

// Gap kinds. They point at different halves of the upgrade and read differently
// to whoever has to fix one, so they are not collapsed into "differs".
const (
	// GapMissing: the fresh scaffold has the file and the upgraded instance does
	// not — the upgrade never delivered something a new adopter gets.
	GapMissing = "missing"
	// GapStale: both have it, bytes differ — the upgraded instance is still
	// carrying an older release's content.
	GapStale = "stale"
	// GapOrphaned: the upgraded instance has a template-owned file the fresh
	// scaffold does not. copier never deletes a file the template dropped, so
	// this is a path that belongs in .template-removals and is not there.
	GapOrphaned = "orphaned"
)

// ConvergenceGaps compares an upgraded instance against a fresh scaffold at the
// same ref, over the files the TEMPLATE owns.
//
// Pure over two path→digest maps and a classifier, so every arm — including the
// ones that need a file to be absent on one side — is unit-testable without
// building two instances. classOf returns the .template-manifest class of a path.
//
// FAIL-CLOSED ON VACUITY is the caller's job (see assertConvergence): an empty
// `fresh` map produces no gaps here, and "no gaps" from a comparison against
// nothing is exactly what a broken scaffold step looks like.
func ConvergenceGaps(fresh, upgraded map[string]string, classOf func(string) string) []ConvergenceGap {
	var gaps []ConvergenceGap
	for path, want := range fresh {
		class := classOf(path)
		if !convergenceAsserted(class) {
			continue
		}
		got, present := upgraded[path]
		switch {
		case !present:
			gaps = append(gaps, ConvergenceGap{Path: path, Class: class, Kind: GapMissing})
		case got != want:
			gaps = append(gaps, ConvergenceGap{Path: path, Class: class, Kind: GapStale})
		}
	}
	for path := range upgraded {
		class := classOf(path)
		if !convergenceAsserted(class) {
			continue
		}
		if _, present := fresh[path]; !present {
			gaps = append(gaps, ConvergenceGap{Path: path, Class: class, Kind: GapOrphaned})
		}
	}
	sort.Slice(gaps, func(i, j int) bool {
		if gaps[i].Kind != gaps[j].Kind {
			return gaps[i].Kind < gaps[j].Kind
		}
		return gaps[i].Path < gaps[j].Path
	})
	return gaps
}

// convergenceAsserted reports whether a class's files must match a fresh
// scaffold byte for byte.
//
// It asks the manifest's own class table rather than naming classes here, so a
// fourth class added upstream is covered by whatever Upgrade action it declares
// instead of being silently skipped by a hardcoded list. An UNRECOGNISED class is
// asserted deliberately: .template-manifest is required to cover the whole
// scaffold (`llz ci template-manifest` fails when it does not), so a path that
// matches no rule is a manifest bug, and skipping it would let a file drop out of
// this comparison by having no rule at all.
func convergenceAsserted(class string) bool {
	c, ok := manifest.LookupClass(class)
	if !ok {
		return true
	}
	return c.Upgrade != manifest.UpgradeRestore
}

// DigestTree walks an instance and returns rel-path → sha256.
//
// Skips .git (the gate commits the scaffold, so the upgraded side has one and the
// fresh side does not — a difference about the harness, not the upgrade) and
// .terraform, matching manifest.ScaffoldFiles' own exclusions so the two views of
// an instance agree on what "the scaffold" is.
func DigestTree(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".terraform":
				return filepath.SkipDir
			}
			return nil
		}
		// Symlinks: digest the LINK, not its target. A dangling one is a real
		// difference and reading through would either error or silently compare
		// the same target twice.
		if d.Type()&fs.ModeSymlink != 0 {
			dst, lerr := os.Readlink(p)
			if lerr != nil {
				return fmt.Errorf("readlink %s: %w", p, lerr)
			}
			rel, _ := filepath.Rel(root, p)
			out[filepath.ToSlash(rel)] = "symlink:" + dst
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		sum := sha256.Sum256(b)
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	return out, err
}

// FormatConvergenceGaps renders the gaps as the gate's failure text.
//
// It names the MECHANISM each kind implicates, because the file alone does not
// say it: a stale `managed` file means the overwrite pass did not run or read a
// render that lacked it, a stale `merge` file means the 3-way merge resolved
// wrong, and an orphan means .template-removals is missing a rule. Those are
// three different fixes in three different files.
func FormatConvergenceGaps(from string, gaps []ConvergenceGap) string {
	var b strings.Builder
	fmt.Fprintf(&b, "converges-with-fresh: an instance upgraded from %s does not match one scaffolded at the target ref (%d file(s)):", from, len(gaps))
	for _, g := range gaps {
		fmt.Fprintf(&b, "\n      %-9s %-8s %s", g.Kind, "["+g.Class+"]", g.Path)
	}
	b.WriteString("\n    An upgraded instance and a fresh one are supposed to be the same instance. Where they differ,\n" +
		"    an adopter who upgraded is running content a new adopter never receives — and nothing else in\n" +
		"    CI compares the two, so the difference persists across every subsequent release.\n" +
		"      missing/stale [managed]  the overwrite pass did not deliver it — see selfupgrade.ManifestPolicy.Apply\n" +
		"                               (a render that SKIPS copier's _tasks is missing whatever those tasks write)\n" +
		"      stale         [merge]    the 3-way merge resolved against the wrong base; with no local edits it\n" +
		"                               must yield the new render exactly\n" +
		"      orphaned                 the template dropped this file and .template-removals has no rule for it,\n" +
		"                               so copier left it behind in every instance that upgraded")
	return b.String()
}
