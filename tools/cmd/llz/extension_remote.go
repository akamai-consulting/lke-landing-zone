package main

// extension_remote.go adds git-pinned remote sources — where extensions come
// FROM (the registry says which are ON). A source is a git repo + pinned ref;
// `llz extension sync` clones it into a gitignored cache and records the resolved
// commit SHA + a content digest in the lock (the go.sum model). A later sync that
// sees a different SHA/digest for the same pin is a hard error — the upstream
// force-moved the ref — unless --update re-pins. Trust model: fetch is gated
// (runGated/--yes), the allowlist is the committed sources list, and the lock
// pins exactly the reviewed bytes.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const extensionCacheDir = ".llz/cache/extensions"

// sourceCacheDir is where a source is cloned (gitignored). Keyed by repo@ref so a
// re-pin fetches into a fresh dir.
func sourceCacheDir(root string, s extSource) string {
	safe := strings.NewReplacer("/", "-", ":", "-").Replace(s.Repo)
	return filepath.Join(root, extensionCacheDir, safe+"@"+s.Ref)
}

// cloneURL lets a local path or explicit scheme through (tests + self-hosted git);
// a bare host/path gets https://.
func cloneURL(repo string) string {
	if strings.Contains(repo, "://") || strings.HasPrefix(repo, "/") || strings.HasPrefix(repo, ".") {
		return repo
	}
	return "https://" + repo
}

// git ops are seams so sync's lock/drift logic unit-tests without a real clone.
var gitClone = func(g globalOpts, url, ref, dir string) error {
	return runGated(g, "git", "clone", "--depth=1", "--branch", ref, url, dir)
}
var gitHead = func(dir string) (string, error) {
	out, err := gitOutput(dir, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// treeDigest is a stable content hash of dir (excluding .git): sorted rel paths,
// each with its file hash. Detects any change to the fetched tree.
func treeDigest(dir string) (string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, rel := range files {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%x\x00", rel, sha256.Sum256(b))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func shortHash(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func runExtensionSync(g globalOpts, root string, update bool) error {
	cfg, err := loadExtConfig(root)
	if err != nil {
		return err
	}
	if len(cfg.Sources) == 0 {
		fmt.Fprintln(os.Stderr, "no sources in "+extensionsConfigFile)
		return nil
	}
	lock := loadExtLock(root)
	for _, s := range cfg.Sources {
		if err := syncSource(g, root, s, &lock, update); err != nil {
			return err
		}
	}
	if g.dryRun {
		return nil
	}
	if err := ensureCacheGitignored(root); err != nil {
		return err
	}
	return saveExtLock(root, lock)
}

// verifyRemoteCache recomputes each locked source's tree digest and compares it to
// the lock — catching a cache tampered or gone stale BETWEEN syncs, at use time
// (the issue's "warm cache verifies digest vs lock"). Sources not yet fetched are
// skipped; a local-only instance has no locked sources, so this is a no-op there.
// (Production would memoize by mtime; a digest per load is fine at this scale.)
func verifyRemoteCache(root string) error {
	for repo, sl := range loadExtLock(root).Sources {
		dir := sourceCacheDir(root, extSource{Repo: repo, Ref: sl.Ref})
		if _, err := os.Stat(dir); err != nil {
			continue // not fetched yet — sync will populate + lock it
		}
		dg, err := treeDigest(dir)
		if err != nil {
			return err
		}
		if dg != sl.Digest {
			return fmt.Errorf("extension source %s cache digest mismatch (lock %s, on-disk %s) — tampered or stale; run `llz extension sync`",
				repo, shortHash(sl.Digest), shortHash(dg))
		}
	}
	return nil
}

// ensureCacheGitignored drops a self-ignoring .gitignore into the cache so fetched
// sources are never committed, regardless of the repo's root .gitignore.
func ensureCacheGitignored(root string) error {
	dir := filepath.Join(root, extensionCacheDir)
	gi := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gi); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(gi, []byte("# fetched extension sources — not committed\n*\n!.gitignore\n"), 0o644)
}

func syncSource(g globalOpts, root string, s extSource, lock *extLock, update bool) error {
	dir := sourceCacheDir(root, s)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil { // cold cache → clone (gated)
		if err := gitClone(g, cloneURL(s.Repo), s.Ref, dir); err != nil {
			return fmt.Errorf("clone %s@%s: %w", s.Repo, s.Ref, err)
		}
		if g.dryRun || !g.yes { // runGated didn't actually fetch
			fmt.Fprintf(os.Stderr, "would fetch %s@%s (re-run with --yes)\n", s.Repo, s.Ref)
			return nil
		}
	}
	sha, err := gitHead(dir)
	if err != nil {
		return fmt.Errorf("rev-parse %s: %w", s.Repo, err)
	}
	dg, err := treeDigest(dir)
	if err != nil {
		return err
	}
	if prev, ok := lock.Sources[s.Repo]; ok && !update {
		if prev.SHA != sha || prev.Digest != dg {
			return fmt.Errorf("source %s drift: lock pins %s/%s but fetched %s/%s — upstream moved %q; re-pin or `--update`",
				s.Repo, shortHash(prev.SHA), shortHash(prev.Digest), shortHash(sha), shortHash(dg), s.Ref)
		}
	}
	if lock.Sources == nil {
		lock.Sources = map[string]sourceLock{}
	}
	lock.Sources[s.Repo] = sourceLock{Ref: s.Ref, SHA: sha, Digest: dg}
	fmt.Fprintf(os.Stderr, "synced %s@%s → %s\n", s.Repo, s.Ref, shortHash(sha))
	return nil
}
