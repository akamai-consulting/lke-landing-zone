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

// The shipped optional built-ins are well-formed: Optional (off by default), pass the
// capability ceiling (built-ins bypass enable-time lint, so guard them here), and carry
// the expected hook shape. This pins the migrated candidates against drift.
func TestShippedOptionalBuiltins(t *testing.T) {
	want := map[string]struct{ files, check, validate, ci, health bool }{
		"lint-yaml":        {files: true, check: true},
		"lint-typos":       {check: true},
		"lint-markdown":    {files: true, check: true},
		"validate-trivy":   {validate: true},
		"scheduled-checks": {ci: true, health: true},
	}
	seen := map[string]bool{}
	for _, b := range builtinExtensions() {
		exp, ok := want[b.Name]
		if !ok {
			continue
		}
		seen[b.Name] = true
		if !b.Manifest.Optional {
			t.Errorf("%s must be optional (off by default, not forced on every instance)", b.Name)
		}
		if findings := lintManifest(b.Manifest); len(findings) > 0 {
			t.Errorf("%s fails the capability ceiling: %v", b.Name, findings)
		}
		if (len(b.Manifest.Files) > 0) != exp.files {
			t.Errorf("%s files presence = %v, want %v", b.Name, len(b.Manifest.Files) > 0, exp.files)
		}
		if (len(b.Manifest.Check) > 0) != exp.check {
			t.Errorf("%s check presence = %v, want %v", b.Name, len(b.Manifest.Check) > 0, exp.check)
		}
		if (len(b.Manifest.Validate) > 0) != exp.validate {
			t.Errorf("%s validate presence = %v, want %v", b.Name, len(b.Manifest.Validate) > 0, exp.validate)
		}
		if (len(b.Manifest.CI) > 0) != exp.ci {
			t.Errorf("%s ci presence = %v, want %v", b.Name, len(b.Manifest.CI) > 0, exp.ci)
		}
		if (len(b.Manifest.Health) > 0) != exp.health {
			t.Errorf("%s health presence = %v, want %v", b.Name, len(b.Manifest.Health) > 0, exp.health)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("shipped built-in %q not found in the embed", name)
		}
	}
}

// missingExtTools reports declared tools whose executable is absent from PATH — the
// readiness gap behind a silent check-skip.
func TestMissingExtTools(t *testing.T) {
	m := extManifest{Tools: []extTool{{Name: "sh"}, {Name: "llz-definitely-absent-xyz", Via: "aqua:x/y"}}}
	miss := missingExtTools(m)
	if len(miss) != 1 || miss[0].Name != "llz-definitely-absent-xyz" {
		t.Fatalf("missingExtTools = %v, want one (llz-definitely-absent-xyz)", miss)
	}
	if fixHint(miss[0]) != "run `llz extension provision`" {
		t.Errorf("a provisionable tool should hint at provision, got %q", fixHint(miss[0]))
	}
}

// Every shipped built-in declares every external tool its check/validate steps invoke,
// and every declared tool that's auto-provisionable carries a pinned mise ref+version.
// `llz` (self) is exempt. This pins tools: to the steps and keeps provisioning reproducible.
func TestShippedBuiltinsDeclareTheirTools(t *testing.T) {
	for _, b := range builtinExtensions() {
		declared := map[string]bool{}
		for _, tl := range b.Manifest.Tools {
			declared[tl.Name] = true
			if tl.Via != "" && tl.Version == "" {
				t.Errorf("built-in %q tool %q has via:%q but no pinned version", b.Name, tl.Name, tl.Via)
			}
		}
		steps := append(append([]extStep{}, b.Manifest.Check...), b.Manifest.Validate...)
		for _, s := range steps {
			if len(s.Argv) == 0 || s.Argv[0] == "llz" {
				continue
			}
			if !declared[s.Argv[0]] {
				t.Errorf("built-in %q step %q invokes %q but does not declare it in tools:", b.Name, s.Name, s.Argv[0])
			}
		}
	}
}

// scheduled-checks proves HookCI + TriggerSchedule end-to-end from a shipped built-in:
// every ci: step carries a cron and renders into the scheduled workflow.
func TestScheduledChecksBuiltinGeneratesScheduledWorkflow(t *testing.T) {
	var m extManifest
	for _, b := range builtinExtensions() {
		if b.Name == "scheduled-checks" {
			m = b.Manifest
		}
	}
	if m.Name == "" {
		t.Fatal("scheduled-checks built-in not found")
	}
	for _, s := range m.CI {
		if s.Schedule == "" {
			t.Errorf("scheduled-checks ci step %q must carry a schedule (TriggerSchedule)", s.Name)
		}
	}
	anchored, scheduled := partitionCIJobs(manifestCIJobs(m))
	if len(anchored) != 0 || len(scheduled) == 0 {
		t.Fatalf("expected all jobs scheduled; got %d anchored, %d scheduled", len(anchored), len(scheduled))
	}
	out, err := renderScheduledWorkflow(scheduled)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "  schedule:", "workflow_dispatch: {}", "llz ci cred-audit")
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

// Reconcile runs contributions in lifecycle (registry) order — derived from the
// phase index, never a hand-kept slice order. With Gate sitting between Configure and
// Bootstrap, the sequence is scaffold → configure → gate → bootstrap.
func TestContributionPhaseOrder(t *testing.T) {
	ordered := orderedContributions()
	got := make([]string, len(ordered))
	for i, c := range ordered {
		got[i] = c.PhaseID()
	}
	if strings.Join(got, ",") != "scaffold,configure,gate,bootstrap" {
		t.Fatalf("lifecycle order = %v", got)
	}
	// indices must be strictly ascending (sorted by the registry, not by luck)
	for i := 1; i < len(ordered); i++ {
		if phaseIndex(ordered[i-1].PhaseID()) >= phaseIndex(ordered[i].PhaseID()) {
			t.Fatalf("contributions not in ascending registry order: %v", got)
		}
	}
}
