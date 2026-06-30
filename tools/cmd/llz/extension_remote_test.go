package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTreeDigestStableAndSensitive(t *testing.T) {
	mk := func(content string) string {
		d := t.TempDir()
		os.MkdirAll(filepath.Join(d, "sub"), 0o755)
		os.WriteFile(filepath.Join(d, "a"), []byte("aaa"), 0o644)
		os.WriteFile(filepath.Join(d, "sub", "b"), []byte(content), 0o644)
		// a .git dir must be ignored
		os.MkdirAll(filepath.Join(d, ".git"), 0o755)
		os.WriteFile(filepath.Join(d, ".git", "HEAD"), []byte("ref: x"), 0o644)
		return d
	}
	d1, _ := treeDigest(mk("same"))
	d2, _ := treeDigest(mk("same"))
	d3, _ := treeDigest(mk("changed"))
	if d1 != d2 {
		t.Fatalf("same content → same digest; got %s vs %s", d1, d2)
	}
	if d1 == d3 {
		t.Fatal("changed content must change the digest (and .git was ignored)")
	}
}

func TestCloneURL(t *testing.T) {
	cases := map[string]string{
		"github.com/apple/llz-recipes": "https://github.com/apple/llz-recipes",
		"https://gitea.local/x/y":      "https://gitea.local/x/y",
		"/tmp/local-remote":            "/tmp/local-remote",
	}
	for in, want := range cases {
		if got := cloneURL(in); got != want {
			t.Errorf("cloneURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// stubGit makes gitClone populate a fake cache dir and gitHead return a fixed
// SHA, so sync's lock/drift logic is testable offline. Restores on cleanup.
func stubGit(t *testing.T, files map[string]string, sha string) {
	t.Helper()
	origClone, origHead := gitClone, gitHead
	gitClone = func(_ globalOpts, _, _, dir string) error {
		for p, c := range files {
			full := filepath.Join(dir, p)
			os.MkdirAll(filepath.Dir(full), 0o755)
			if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
				return err
			}
		}
		os.MkdirAll(filepath.Join(dir, ".git"), 0o755) // make the cold-cache check pass
		return os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("x"), 0o644)
	}
	gitHead = func(string) (string, error) { return sha, nil }
	t.Cleanup(func() { gitClone, gitHead = origClone, origHead })
}

func TestSyncLocksThenDetectsDrift(t *testing.T) {
	root := t.TempDir()
	src := extSource{Repo: "github.com/apple/llz-recipes", Ref: "v1.0.0"}
	if err := saveExtConfig(root, extConfig{Sources: []extSource{src}}); err != nil {
		t.Fatal(err)
	}

	// first sync: clone (stubbed), lock the SHA+digest.
	stubGit(t, map[string]string{"renovate/recipe.yaml": "name: renovate\n"}, "sha-AAAA0000")
	if err := runExtensionSync(globalOpts{yes: true}, root, false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	lock := loadExtLock(root)
	if lock.Sources[src.Repo].SHA != "sha-AAAA0000" {
		t.Fatalf("lock SHA = %q", lock.Sources[src.Repo].SHA)
	}

	// re-sync, same bytes → ok (cache warm, gitHead still AAAA).
	if err := runExtensionSync(globalOpts{yes: true}, root, false); err != nil {
		t.Fatalf("re-sync should be clean: %v", err)
	}

	// upstream force-moved the ref: same cache files but a new HEAD → drift error.
	gitHead = func(string) (string, error) { return "sha-BBBB1111", nil }
	if err := runExtensionSync(globalOpts{yes: true}, root, false); err == nil {
		t.Fatal("a moved SHA under the same pin must be a hard error")
	}
	// --update re-pins.
	if err := runExtensionSync(globalOpts{yes: true}, root, true); err != nil {
		t.Fatalf("--update should re-pin: %v", err)
	}
	if loadExtLock(root).Sources[src.Repo].SHA != "sha-BBBB1111" {
		t.Fatal("--update should have re-pinned the SHA")
	}
}

func TestSyncRefusesWithoutYes(t *testing.T) {
	root := t.TempDir()
	saveExtConfig(root, extConfig{Sources: []extSource{{Repo: "github.com/apple/x", Ref: "v1"}}})
	stubGit(t, map[string]string{"a/recipe.yaml": "name: a\n"}, "sha")
	// gitClone seam still runs, but runExtensionSync's gating short-circuits before
	// locking when --yes is absent; the lock must stay empty.
	if err := runExtensionSync(globalOpts{yes: false}, root, false); err != nil {
		t.Fatalf("sync without --yes should be a no-op, not an error: %v", err)
	}
	if len(loadExtLock(root).Sources) != 0 {
		t.Fatal("nothing should be locked until fetched with --yes")
	}
}

func TestSyncGitignoresCache(t *testing.T) {
	root := t.TempDir()
	src := extSource{Repo: "github.com/apple/x", Ref: "v1"}
	saveExtConfig(root, extConfig{Sources: []extSource{src}})
	stubGit(t, map[string]string{"a/recipe.yaml": "name: a\n"}, "sha")
	if err := runExtensionSync(globalOpts{yes: true}, root, false); err != nil {
		t.Fatal(err)
	}
	gi := filepath.Join(root, extensionCacheDir, ".gitignore")
	b, err := os.ReadFile(gi)
	if err != nil {
		t.Fatalf("cache .gitignore should exist: %v", err)
	}
	if !strings.Contains(string(b), "*") {
		t.Fatalf("cache .gitignore should ignore everything; got %q", b)
	}
}

// First-enable of a remote extension is gated behind --yes (trust model).
func TestEnableRemoteGatedWithoutYes(t *testing.T) {
	root := t.TempDir()
	src := extSource{Repo: "github.com/apple/llz-recipes", Ref: "v1"}
	saveExtConfig(root, extConfig{Sources: []extSource{src}})
	cache := sourceCacheDir(root, src)
	os.MkdirAll(filepath.Join(cache, "renovate"), 0o755)
	os.WriteFile(filepath.Join(cache, "renovate", extensionManifest),
		[]byte("schemaVersion: 3\nname: renovate\nshort: x\nkind: tool\nstage: universal\n"), 0o644)

	if err := runExtensionEnable(globalOpts{yes: false}, root, "renovate"); err == nil {
		t.Fatal("enabling a remote extension without --yes should be refused")
	}
	if cfg, _ := loadExtConfig(root); len(cfg.Enabled) != 0 {
		t.Fatal("a gated enable must not be recorded")
	}
	// with --yes it proceeds
	if err := runExtensionEnable(globalOpts{yes: true}, root, "renovate"); err != nil {
		t.Fatalf("enable --yes should succeed: %v", err)
	}
	if cfg, _ := loadExtConfig(root); len(cfg.Enabled) != 1 {
		t.Fatal("enable --yes should record it")
	}
}

// verify-on-use: a cache tampered between syncs is caught when the loader reads it.
func TestVerifyRemoteCacheDetectsTamper(t *testing.T) {
	root := t.TempDir()
	src := extSource{Repo: "github.com/apple/x", Ref: "v1"}
	saveExtConfig(root, extConfig{Sources: []extSource{src}})
	stubGit(t, map[string]string{"a/recipe.yaml": "name: a\n"}, "sha")
	if err := runExtensionSync(globalOpts{yes: true}, root, false); err != nil {
		t.Fatal(err)
	}
	// clean cache verifies
	if err := verifyRemoteCache(root); err != nil {
		t.Fatalf("freshly-synced cache should verify: %v", err)
	}
	// tamper a file in the cache → digest mismatch
	if err := os.WriteFile(filepath.Join(sourceCacheDir(root, src), "a", "recipe.yaml"), []byte("name: EVIL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyRemoteCache(root); err == nil {
		t.Fatal("a tampered cache must fail verification")
	}
	// and the loader refuses to read it
	saveExtConfig(root, extConfig{Sources: []extSource{src}, Enabled: []string{"a"}})
	if _, err := loadEnabledExtensions(root); err == nil {
		t.Fatal("loadEnabledExtensions must reject a tampered cache")
	}
}

func TestEnableResolvesFromSyncedSource(t *testing.T) {
	root := t.TempDir()
	src := extSource{Repo: "github.com/apple/llz-recipes", Ref: "v1"}
	saveExtConfig(root, extConfig{Sources: []extSource{src}})
	// place a synced extension directly in the cache (as sync would).
	cache := sourceCacheDir(root, src)
	os.MkdirAll(filepath.Join(cache, "renovate"), 0o755)
	os.WriteFile(filepath.Join(cache, "renovate", extensionManifest),
		[]byte("schemaVersion: 3\nname: renovate\nshort: x\nkind: tool\nstage: universal\n"), 0o644)

	cfg, _ := loadExtConfig(root)
	if dir, ok := resolveExtensionDir(root, cfg, "renovate"); !ok || dir != filepath.Join(cache, "renovate") {
		t.Fatalf("resolveExtensionDir should find the source ext; got %q, %v", dir, ok)
	}
}
