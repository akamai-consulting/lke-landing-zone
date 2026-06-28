package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A built-in extension loads from embed.FS and carries an origin-erasing fsys.
func TestBuiltinExtensionsLoad(t *testing.T) {
	bs := builtinExtensions()
	if len(bs) == 0 {
		t.Fatal("expected at least one built-in extension")
	}
	var ga *Extension
	for i := range bs {
		if bs[i].Name == "gitattributes" {
			ga = &bs[i]
		}
	}
	if ga == nil {
		t.Fatal("built-in gitattributes not found")
	}
	if ga.Source != "builtin" || ga.Dir != "" || ga.fsys == nil {
		t.Fatalf("built-in should carry embed fsys + builtin source, no dir: %+v", ga)
	}
}

// Origin-erasure: a built-in's files: render through the SAME applyExtensionFiles
// path as a local extension, reading from its embed fsys.
func TestBuiltinScaffoldsThroughSharedPath(t *testing.T) {
	root := t.TempDir()
	exts := builtinExtensions()
	var did bool
	for _, e := range exts {
		if e.Name != "gitattributes" {
			continue
		}
		if err := applyExtensionFiles(globalOpts{}, e, root, false); err != nil {
			t.Fatalf("apply built-in: %v", err)
		}
		did = true
	}
	if !did {
		t.Skip("no gitattributes built-in")
	}
	if _, err := os.Stat(filepath.Join(root, ".gitattributes")); err != nil {
		t.Fatalf("built-in should have scaffolded .gitattributes from embed: %v", err)
	}
}

// loadAllExtensions = built-ins + enabled, built-ins first.
func TestLoadAllExtensionsIncludesBuiltins(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "local1", "schemaVersion: 2\nname: local1\nshort: x\nkind: tool\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"local1"}})
	all, err := loadAllExtensions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 2 || all[len(all)-1].Name != "local1" {
		t.Fatalf("expected built-ins then local1; got %v", names(all))
	}
	if all[0].Source != "builtin" {
		t.Fatalf("built-ins should come first; got %q", all[0].Source)
	}
}

// reconcile runs every phase, and the Gate (check:) actually EXECUTES — proven by
// a check whose tool exists succeeding, and a failing check aborting reconcile.
func TestReconcileRunsGate(t *testing.T) {
	root := t.TempDir()
	// `true` is on PATH and exits 0; reconcile's gate should run it cleanly.
	installExt(t, root, "ok", "schemaVersion: 2\nname: ok\nshort: x\nkind: tool\ncheck:\n  - {name: c, argv: [true]}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"ok"}})
	if err := runExtensionReconcile(globalOpts{}, root, false); err != nil {
		t.Fatalf("reconcile with a passing gate should succeed: %v", err)
	}

	// `false` exits 1 → the gate fails → reconcile aborts.
	installExt(t, root, "bad", "schemaVersion: 2\nname: bad\nshort: x\nkind: tool\ncheck:\n  - {name: c, argv: [false]}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"ok", "bad"}})
	if err := runExtensionReconcile(globalOpts{}, root, false); err == nil {
		t.Fatal("a failing gate check must abort reconcile (the Gate now executes)")
	}
}

// reconcile scaffolds files: across the set (built-in + enabled) in one pass.
func TestReconcileScaffoldsFiles(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "cfg",
		"schemaVersion: 2\nname: cfg\nshort: x\nkind: tool\nfiles:\n  - {src: f, dst: OUT}\n",
		map[string]string{"f": "hi\n"})
	saveExtConfig(root, extConfig{Enabled: []string{"cfg"}})
	if err := runExtensionReconcile(globalOpts{}, root, false); err != nil {
		t.Fatal(err)
	}
	// the enabled extension's file AND the built-in's .gitattributes both landed
	for _, p := range []string{"OUT", ".gitattributes"} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Fatalf("reconcile should have scaffolded %s: %v", p, err)
		}
	}
}

func names(es []Extension) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

// The runLint tail: runExtensionGate runs enabled checks, skips missing tools, and
// tolerates a broken config (warns, doesn't wedge the fast gate).
func TestRunExtensionGate(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "ok", "schemaVersion: 2\nname: ok\nshort: x\nkind: tool\ncheck:\n  - {name: c, argv: [true]}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"ok"}})
	if err := runExtensionGate(globalOpts{}, root); err != nil {
		t.Fatalf("passing check should pass the gate: %v", err)
	}
	// a failing check fails the gate
	installExt(t, root, "bad", "schemaVersion: 2\nname: bad\nshort: x\nkind: tool\ncheck:\n  - {name: c, argv: [false]}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"ok", "bad"}})
	if err := runExtensionGate(globalOpts{}, root); err == nil {
		t.Fatal("a failing extension check must fail the gate (runLint tail)")
	}
	// a missing-tool check skips (never wedges)
	installExt(t, root, "skip", "schemaVersion: 2\nname: skip\nshort: x\nkind: tool\ncheck:\n  - {name: c, argv: [llz-no-such-tool-xyz]}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"ok", "skip"}})
	if err := runExtensionGate(globalOpts{}, root); err != nil {
		t.Fatalf("a missing tool should skip, not fail: %v", err)
	}
	// a broken config warns but does not wedge the gate
	saveExtConfig(root, extConfig{Enabled: []string{"ghost"}})
	if err := runExtensionGate(globalOpts{}, root); err != nil {
		t.Fatalf("a load error should warn, not wedge lint: %v", err)
	}
}

func TestContributionPhaseOrder(t *testing.T) {
	got := make([]string, len(contributions))
	for i, c := range contributions {
		got[i] = c.Phase()
	}
	if strings.Join(got, ",") != "Configure,Scaffold,Bootstrap,Gate" {
		t.Fatalf("phase order = %v", got)
	}
}
