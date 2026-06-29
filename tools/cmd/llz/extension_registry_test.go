package main

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// The third tier: an optional built-in ships with the binary but is OFF until enabled,
// then loads + scaffolds from the embed like any extension; disabling drops it again.
// Always-on built-ins stay always-on. (Seamed via builtinExtensionsFn — no real embed.)
func TestOptionalBuiltinTier(t *testing.T) {
	root := t.TempDir()
	orig := builtinExtensionsFn
	t.Cleanup(func() { builtinExtensionsFn = orig })
	builtinExtensionsFn = func() []Extension {
		return []Extension{
			{Name: "core-hygiene", Source: "builtin", Manifest: extManifest{Name: "core-hygiene", Short: "x", Kind: "tool"}},
			{Name: "optx", Source: "builtin", fsys: fstest.MapFS{"hello.txt": &fstest.MapFile{Data: []byte("hi\n")}},
				Manifest: extManifest{Name: "optx", Short: "optional one", Kind: "tool", Optional: true,
					Files: []extFile{{Src: "hello.txt", Dst: "hello.txt"}}}},
		}
	}
	has := func(exts []Extension, name string) bool {
		for _, e := range exts {
			if e.Name == name {
				return true
			}
		}
		return false
	}

	// not enabled: always-on present, optional absent
	all, err := loadAllExtensions(root)
	if err != nil {
		t.Fatal(err)
	}
	if !has(all, "core-hygiene") {
		t.Error("always-on built-in must always load")
	}
	if has(all, "optx") {
		t.Error("optional built-in must NOT load until enabled")
	}

	// enable → records it AND scaffolds from the embed
	if err := runExtensionEnable(globalOpts{yes: true}, root, "optx"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "hello.txt")); err != nil {
		t.Fatalf("enable should scaffold from the embed: %v", err)
	}

	// now loaded by both views, exactly once
	all, _ = loadAllExtensions(root)
	count := 0
	for _, e := range all {
		if e.Name == "optx" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("enabled optional built-in loaded %d times, want exactly 1", count)
	}
	if en, _ := loadEnabledExtensions(root); !has(en, "optx") {
		t.Error("enabled optional built-in must appear in the enabled set (seed/ci/commands see it)")
	}

	// disable → absent again
	if err := runExtensionDisable(globalOpts{yes: true}, root, "optx"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if all, _ = loadAllExtensions(root); has(all, "optx") {
		t.Error("disabled optional built-in must not load")
	}
}

// installExt writes an extension under <root>/extensions/<name>/.
func installExt(t *testing.T, root, name, manifest string, srcs map[string]string) {
	t.Helper()
	dir := filepath.Join(root, "extensions", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
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
}

func TestEnableScaffoldsAndDisablePersists(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "codeowners", filesManifest,
		map[string]string{"tpl/CODEOWNERS": "* @<@ .name @>-team\n"})

	if err := runExtensionEnable(globalOpts{}, root, "codeowners"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// config records it
	cfg, _ := loadExtConfig(root)
	if len(cfg.Enabled) != 1 || cfg.Enabled[0] != "codeowners" {
		t.Fatalf("enabled = %v, want [codeowners]", cfg.Enabled)
	}
	// enable scaffolded the file
	if _, err := os.Stat(filepath.Join(root, ".github/CODEOWNERS")); err != nil {
		t.Fatalf("enable should have scaffolded the file: %v", err)
	}
	// loader resolves it
	exts, err := loadEnabledExtensions(root)
	if err != nil || len(exts) != 1 || exts[0].Name != "codeowners" {
		t.Fatalf("loadEnabledExtensions = %v, %v", exts, err)
	}
	// disable removes from config, leaves the file
	if err := runExtensionDisable(globalOpts{}, root, "codeowners"); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := loadExtConfig(root); len(cfg.Enabled) != 0 {
		t.Fatalf("disable should empty enabled, got %v", cfg.Enabled)
	}
	if _, err := os.Stat(filepath.Join(root, ".github/CODEOWNERS")); err != nil {
		t.Fatal("disable must leave scaffolded files in place")
	}
}

func TestEnableRejectsFailingCeiling(t *testing.T) {
	root := t.TempDir()
	// kind:check with no tests fails lintKind → enable must refuse.
	installExt(t, root, "bad", "schemaVersion: 2\nname: bad\nshort: x\nkind: check\n", nil)
	if err := runExtensionEnable(globalOpts{}, root, "bad"); err == nil {
		t.Fatal("enable should refuse an extension that fails the ceiling")
	}
	if cfg, _ := loadExtConfig(root); len(cfg.Enabled) != 0 {
		t.Fatal("a refused enable must not be recorded")
	}
}

func TestApplyAllOverEnabledSet(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "a", "schemaVersion: 2\nname: a\nshort: x\nkind: tool\nfiles:\n  - {src: f, dst: A}\n",
		map[string]string{"f": "aye\n"})
	installExt(t, root, "b", "schemaVersion: 2\nname: b\nshort: x\nkind: tool\nfiles:\n  - {src: f, dst: B}\n",
		map[string]string{"f": "bee\n"})
	if err := saveExtConfig(root, extConfig{Enabled: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	if err := runExtensionApplyAll(globalOpts{}, root, false); err != nil {
		t.Fatalf("apply all: %v", err)
	}
	for _, f := range []string{"A", "B"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Fatalf("expected %s scaffolded: %v", f, err)
		}
	}
	// --check over the set is clean right after apply
	if err := runExtensionApplyAll(globalOpts{}, root, true); err != nil {
		t.Fatalf("apply-all --check should pass: %v", err)
	}
}

func TestEnabledCIJobsFromSet(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "obs", "schemaVersion: 2\nname: obs\nshort: x\nkind: tool\nci:\n  - {name: dash, anchor: post-converge, argv: [llz, ci, dash]}\n", nil)
	if err := saveExtConfig(root, extConfig{Enabled: []string{"obs"}}); err != nil {
		t.Fatal(err)
	}
	jobs, err := enabledCIJobs(root)
	if err != nil || len(jobs) != 1 || jobs[0].Ext != "obs" || jobs[0].Anchor != anchorPostConverge {
		t.Fatalf("enabledCIJobs = %+v, %v", jobs, err)
	}
}

func TestEnabledExtCommandsGathersAndDefaultsShort(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "ops",
		"schemaVersion: 2\nname: ops\nshort: x\nkind: tool\ncommands:\n  - {name: drain, argv: [kubectl, drain]}\n  - {name: status, short: cluster status, argv: [llz, status]}\n", nil)
	if err := saveExtConfig(root, extConfig{Enabled: []string{"ops"}}); err != nil {
		t.Fatal(err)
	}
	cmds, err := enabledExtCommands(root)
	if err != nil || len(cmds) != 2 {
		t.Fatalf("enabledExtCommands = %+v, %v", cmds, err)
	}
	// short defaulted when absent, preserved when set
	byName := map[string]extCommand{}
	for _, c := range cmds {
		byName[c.Name] = c
	}
	if byName["drain"].Short == "" || byName["status"].Short != "cluster status" {
		t.Fatalf("short handling wrong: %+v", byName)
	}
}

// the argv-only ceiling extends to commands: — inline shell is rejected.
func TestLintRejectsInlineShellCommand(t *testing.T) {
	m := extManifest{Name: "x", Short: "y", Kind: "tool",
		Commands: []extCommand{{Name: "bad", Argv: []string{"bash", "-c", "rm -rf /"}}}}
	if findings := lintManifest(m); len(findings) != 1 {
		t.Fatalf("expected 1 finding for an inline-shell command, got %v", findings)
	}
	ok := extManifest{Name: "x", Short: "y", Kind: "tool",
		Commands: []extCommand{{Name: "good", Argv: []string{"kubectl", "get", "pods"}}}}
	if findings := lintManifest(ok); len(findings) != 0 {
		t.Fatalf("a plain argv command should pass, got %v", findings)
	}
}

func TestLoadEnabledMissingErrors(t *testing.T) {
	root := t.TempDir()
	if err := saveExtConfig(root, extConfig{Enabled: []string{"ghost"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEnabledExtensions(root); err == nil {
		t.Fatal("an enabled-but-missing extension should error")
	}
}
