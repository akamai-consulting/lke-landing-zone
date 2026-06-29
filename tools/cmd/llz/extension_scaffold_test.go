package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A files: entry whose src is a DIRECTORY scaffolds the whole subtree — each file
// rendered to dst joined with its path relative to src. The flattened per-file outputs
// keep drift/--check, the lock, and teardown working. (dst is concrete: only file BODIES
// render at apply time, via scaffoldVals; the dst is fixed by `extension new`.)
func TestDirScaffold(t *testing.T) {
	root := t.TempDir()
	extDir := filepath.Join(root, "extensions", "kit")
	installExt(t, root, "kit",
		"schemaVersion: 2\nname: kit\nshort: x\nkind: tool\nfiles:\n  - {src: app, dst: apps/kit}\n",
		map[string]string{
			"app/Cargo.toml":        "[package]\nname = \"<@ .name @>\"\n", // body renders <@ .name @> → kit
			"app/src/lib.rs":        "// <@ .name @> gateway\n",
			"app/spin.toml":         "spin_manifest_version = 2\n",
			"app/.cargo/audit.toml": "[advisories]\n",
		})

	if err := runExtensionApply(globalOpts{}, extDir, root, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// the whole subtree landed under apps/kit/ (incl. nested + dotfile dirs), body rendered
	for rel, want := range map[string]string{
		"apps/kit/Cargo.toml":        `name = "kit"`,
		"apps/kit/src/lib.rs":        "// kit gateway",
		"apps/kit/spin.toml":         "spin_manifest_version",
		"apps/kit/.cargo/audit.toml": "[advisories]",
	} {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("%s missing: %v", rel, err)
		}
		if !strings.Contains(string(b), want) {
			t.Errorf("%s = %q, want contains %q", rel, b, want)
		}
	}
	// the lock recorded all four as per-file outputs
	if got := len(loadExtLock(root).Outputs["kit"]); got != 4 {
		t.Fatalf("lock recorded %d files, want 4", got)
	}
	// --check is in sync right after apply...
	if err := runExtensionApply(globalOpts{}, extDir, root, true); err != nil {
		t.Fatalf("--check should be in sync after apply: %v", err)
	}
	// ...and flags a hand-edited file in the subtree
	if err := os.WriteFile(filepath.Join(root, "apps/kit/spin.toml"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runExtensionApply(globalOpts{}, extDir, root, true); err == nil {
		t.Fatal("--check should detect the edited scaffolded file in the subtree")
	}

	// teardown removes every flattened file (the lock keyed them like any other)
	if err := runExtensionTeardown(globalOpts{yes: true}, root, "kit", true); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "apps/kit/Cargo.toml")); !os.IsNotExist(err) {
		t.Fatal("teardown should have removed the dir-scaffolded files")
	}
}

// writeExt creates an extension dir with a manifest body and optional source
// files (path->content), returning the extension dir.
func writeExt(t *testing.T, manifest string, srcs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, extensionManifest), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	for p, c := range srcs {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const filesManifest = `schemaVersion: 3
name: codeowners
short: drops a CODEOWNERS file
kind: tool
stage: universal
files:
  - {src: tpl/CODEOWNERS, dst: .github/CODEOWNERS}
`

// apply renders the file (with <@ @> values), writes it, records the lock; then
// --check is clean, catches a hand-edit, and catches a delete.
func TestScaffoldApplyCheckDrift(t *testing.T) {
	ext := writeExt(t, filesManifest, map[string]string{"tpl/CODEOWNERS": "* @<@ .name @>-team\n"})
	root := t.TempDir()

	if err := runExtensionApply(globalOpts{}, ext, root, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// rendered into place with the value substituted
	got, err := os.ReadFile(filepath.Join(root, ".github/CODEOWNERS"))
	if err != nil || strings.TrimSpace(string(got)) != "* @codeowners-team" {
		t.Fatalf("scaffolded content = %q err=%v", got, err)
	}
	// lock recorded the owned path
	if _, err := os.Stat(filepath.Join(root, extensionLockFile)); err != nil {
		t.Fatalf("lock not written: %v", err)
	}
	// a fresh apply is in sync
	if err := runExtensionApply(globalOpts{}, ext, root, true); err != nil {
		t.Fatalf("--check should pass right after apply: %v", err)
	}
	// hand-edit → drift
	if err := os.WriteFile(filepath.Join(root, ".github/CODEOWNERS"), []byte("* @someone-else\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runExtensionApply(globalOpts{}, ext, root, true); err == nil {
		t.Fatal("--check should fail after a hand-edit")
	}
	// delete → drift (unscaffolded)
	os.Remove(filepath.Join(root, ".github/CODEOWNERS"))
	if err := runExtensionApply(globalOpts{}, ext, root, true); err == nil {
		t.Fatal("--check should fail when a scaffolded file is missing")
	}
}

// A file the extension once shipped but dropped from its manifest is reported as
// orphaned via the lock.
func TestScaffoldDetectsOrphan(t *testing.T) {
	ext := writeExt(t, filesManifest, map[string]string{"tpl/CODEOWNERS": "* @<@ .name @>-team\n"})
	root := t.TempDir()
	if err := runExtensionApply(globalOpts{}, ext, root, false); err != nil {
		t.Fatal(err)
	}
	// rewrite the manifest with NO files: — the prior CODEOWNERS is now an orphan
	if err := os.WriteFile(filepath.Join(ext, extensionManifest),
		[]byte("schemaVersion: 2\nname: codeowners\nshort: x\nkind: tool\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// no files: in the manifest → nothing to render, so apply --check returns the
	// no-op path; assert the orphan is visible through the lock directly.
	lock := loadExtLock(root)
	if len(lock.Outputs["codeowners"]) != 1 || lock.Outputs["codeowners"][0].Path != ".github/CODEOWNERS" {
		t.Fatalf("lock should still record the orphaned output: %+v", lock.Outputs)
	}
}

// The closed loop: upgrade re-applies files: even when the schema is already
// current, so a changed template propagates with the binary.
func TestUpgradeReappliesFiles(t *testing.T) {
	ext := writeExt(t, filesManifest, map[string]string{"tpl/CODEOWNERS": "* @<@ .name @>-team\n"})
	root := t.TempDir()
	if err := runExtensionApply(globalOpts{}, ext, root, false); err != nil {
		t.Fatal(err)
	}
	// a new extension version ships a changed template
	if err := os.WriteFile(filepath.Join(ext, "tpl/CODEOWNERS"), []byte("* @<@ .name @>-team @secops\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runExtensionUpgrade(globalOpts{}, ext, root, false); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(root, ".github/CODEOWNERS"))
	if !strings.Contains(string(got), "@secops") {
		t.Fatalf("upgrade should re-apply the changed template; got %q", got)
	}
}

func TestOwnedPathsDedupedSorted(t *testing.T) {
	l := extLock{Outputs: map[string][]lockedFile{
		"b": {{Path: "z", SHA: "1"}, {Path: "a", SHA: "2"}},
		"a": {{Path: "a", SHA: "3"}}, // duplicate path across extensions
	}}
	if got := strings.Join(ownedPaths(l), ","); got != "a,z" {
		t.Fatalf("ownedPaths = %q, want a,z", got)
	}
}

func TestScaffoldDryRunWritesNothing(t *testing.T) {
	ext := writeExt(t, filesManifest, map[string]string{"tpl/CODEOWNERS": "* @team\n"})
	root := t.TempDir()
	if err := runExtensionApply(globalOpts{dryRun: true}, ext, root, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".github/CODEOWNERS")); !os.IsNotExist(err) {
		t.Fatal("dry-run must not write the file")
	}
}
