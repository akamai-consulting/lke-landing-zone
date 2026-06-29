package main

// extension_adopt.go closes the copier→extension migration hazard (issue #10). When a
// file moves OUT of instance-template/ and INTO an extension, an existing instance still
// has the file (from the old template) but has NOT enabled the extension — so a later
// `copier update` deletes it (it vanished from the template) and nothing restores it.
//
// Adoption detects this: an AVAILABLE-but-not-enabled extension whose files: outputs are
// ALL already present on disk, and that we have NEVER recorded in the lock, is evidence the
// instance already carries that capability (it came from the template). Adopting it enables
// the extension and records the present files in the lock. Recording them BEFORE the
// `copier update` matters: runUpgrade excludes `ownedPaths(lock)` from copier, so once a
// path is owned, copier never deletes it — preserving even an operator-customized `seed`
// file across the handoff.
//
// The lock check is the discriminator that keeps adoption from fighting a deliberate
// disable: `disable` leaves files in place but their entries stay in the lock, so a
// disabled extension is NOT re-adopted; only a never-applied (migrated-in) file is.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// adoptionCandidates returns available extensions (optional built-ins + local dirs) that
// are not enabled, declare files:, have every file destination present on disk, and have NO
// existing lock entry (never applied by us → migrated in from the template). Pure-ish read.
func adoptionCandidates(root string) ([]Extension, error) {
	cfg, err := loadExtConfig(root)
	if err != nil {
		return nil, err
	}
	enabled := map[string]bool{}
	for _, n := range cfg.Enabled {
		enabled[n] = true
	}
	lock := loadExtLock(root)

	var avail []Extension
	for _, b := range builtinExtensionsFn() { // optional built-ins not yet enabled
		if b.Manifest.Optional && !enabled[b.Name] {
			avail = append(avail, b)
		}
	}
	base := filepath.Join(root, cfg.extDir())
	if entries, derr := os.ReadDir(base); derr == nil {
		for _, e := range entries {
			if !e.IsDir() || enabled[e.Name()] {
				continue
			}
			if ext, ferr := extensionFromDir(filepath.Join(base, e.Name())); ferr == nil {
				avail = append(avail, ext)
			}
		}
	}

	var candidates []Extension
	for _, e := range avail {
		if len(e.Manifest.Files) == 0 {
			continue
		}
		if _, recorded := lock.Outputs[e.Name]; recorded {
			continue // we applied this before (e.g. it was disabled) — not a fresh migration
		}
		files, ferr := renderScaffold(e, root)
		if ferr != nil || len(files) == 0 {
			continue
		}
		allPresent := true
		for _, f := range files {
			if _, serr := os.Stat(filepath.Join(root, f.Dst)); serr != nil {
				allPresent = false
				break
			}
		}
		if allPresent {
			candidates = append(candidates, e)
		}
	}
	return candidates, nil
}

// stampAdopted records an adopted extension's present files in the lock using their CURRENT
// on-disk digests (non-destructive — adoption never overwrites; it claims ownership of what
// is already there, so the subsequent copier-exclude preserves it).
func stampAdopted(root, name string, files []renderedFile) error {
	lock := loadExtLock(root)
	var locked []lockedFile
	for _, f := range files {
		got, err := os.ReadFile(filepath.Join(root, f.Dst))
		if err != nil {
			return err
		}
		locked = append(locked, lockedFile{Path: f.Dst, SHA: digest(got), Mode: f.Mode})
	}
	lock.Outputs[name] = locked
	return saveExtLock(root, lock)
}

// runExtensionAdopt enables every adoption candidate and records its present files in the
// lock. --dry-run lists them and writes nothing. Fired automatically at the top of
// `llz upgrade` (before the copier update) and available as `llz extension adopt`.
func runExtensionAdopt(g globalOpts, root string) error {
	cands, err := adoptionCandidates(root)
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		fmt.Fprintln(os.Stderr, "no extensions to adopt (no available extension's files are already present)")
		return nil
	}
	cfg, err := loadExtConfig(root)
	if err != nil {
		return err
	}
	for _, e := range cands {
		files, _ := renderScaffold(e, root)
		customized := 0
		for _, f := range files {
			if f.Mode != "seed" {
				if got, rerr := os.ReadFile(filepath.Join(root, f.Dst)); rerr == nil && digest(got) != f.SHA {
					customized++
				}
			}
		}
		if g.dryRun {
			fmt.Fprintf(os.Stderr, "→ would adopt %q (%d present file(s)%s)\n", e.Name, len(files), customizedNote(customized))
			continue
		}
		if !slices.Contains(cfg.Enabled, e.Name) {
			cfg.Enabled = append(cfg.Enabled, e.Name)
		}
		if err := stampAdopted(root, e.Name, files); err != nil {
			return fmt.Errorf("adopt %q: %w", e.Name, err)
		}
		fmt.Fprintf(os.Stderr, "adopted %q — enabled + recorded %d existing file(s)%s\n", e.Name, len(files), customizedNote(customized))
	}
	if g.dryRun {
		return nil
	}
	if err := saveExtConfig(root, cfg); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "run `llz extension exclude` and add the block to copier.yml so `copier update` keeps these paths")
	return nil
}

// customizedNote annotates an adoption with how many of its managed files differ from the
// extension's rendered output (an operator-edited managed file will be reconciled to the
// extension's canonical version on the next apply; a seed file is left alone).
func customizedNote(customized int) string {
	if customized == 0 {
		return ""
	}
	return fmt.Sprintf(", %d managed file(s) differ from canonical — apply will reconcile", customized)
}
