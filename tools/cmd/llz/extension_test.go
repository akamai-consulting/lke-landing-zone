package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestIsInlineShell(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"named tool", []string{"renovate-config-validator"}, false},
		{"go run entrypoint", []string{"go", "run", "./cmd/check"}, false},
		{"bash -c smuggles logic", []string{"bash", "-c", "for f in *; do …; done"}, true},
		{"sh -c smuggles logic", []string{"sh", "-c", "echo hi"}, true},
		{"sh without -c is a real tool call", []string{"sh", "deploy.sh"}, false},
		{"abs-path bash -c still caught", []string{"/bin/bash", "-c", "x"}, true},
		{"empty", nil, false},
	}
	for _, c := range cases {
		if got := isInlineShell(c.argv); got != c.want {
			t.Errorf("%s: isInlineShell(%v) = %v, want %v", c.name, c.argv, got, c.want)
		}
	}
}

func TestLintManifest(t *testing.T) {
	cases := []struct {
		name      string
		m         extManifest
		wantCount int
	}{
		{"clean tool", extManifest{Name: "renovate", Short: "deps", Kind: "tool",
			Check: []extStep{{Name: "v", Argv: []string{"renovate-config-validator"}}}}, 0},
		{"missing name+short", extManifest{Kind: "check"}, 2},
		{"unknown kind", extManifest{Name: "x", Short: "y", Kind: "plugin"}, 1},
		{"empty kind", extManifest{Name: "x", Short: "y"}, 1},
		{"inline shell rejected", extManifest{Name: "x", Short: "y", Kind: "check",
			CI: []extStep{{Name: "deploy", Argv: []string{"bash", "-c", "kubectl apply"}}}}, 1},
		{"empty argv rejected", extManifest{Name: "x", Short: "y", Kind: "check",
			Check: []extStep{{Name: "v", Argv: nil}}}, 1},
	}
	for _, c := range cases {
		if got := lintManifest(c.m); len(got) != c.wantCount {
			t.Errorf("%s: %d findings, want %d (%v)", c.name, len(got), c.wantCount, got)
		}
	}
}

func TestLintKind(t *testing.T) {
	cases := []struct {
		name      string
		kind      string
		hasTests  bool
		wantCount int
	}{
		{"check with tests ok", "check", true, 0},
		{"check without tests fails", "check", false, 1},
		{"tool without tests ok", "tool", false, 0},
	}
	for _, c := range cases {
		if got := lintKind(extManifest{Kind: c.kind}, c.hasTests); len(got) != c.wantCount {
			t.Errorf("%s: %d findings, want %d (%v)", c.name, len(got), c.wantCount, got)
		}
	}
}

// TestScaffoldThenLint is the experiment's headline assertion: a freshly
// scaffolded extension passes its own ceiling check out of the box — the
// gradient starts at "green", not "blank page".
func TestScaffoldThenLint(t *testing.T) {
	for _, kind := range []string{"check", "tool", "observability"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := runExtensionNew(globalOpts{}, "demo", dir, kind); err != nil {
				t.Fatalf("scaffold: %v", err)
			}
			if err := runExtensionLint(filepath.Join(dir, "demo")); err != nil {
				t.Fatalf("scaffolded %s extension failed its own lint: %v", kind, err)
			}
		})
	}
}

func TestScaffoldRejectsUnknownKind(t *testing.T) {
	if err := runExtensionNew(globalOpts{}, "demo", t.TempDir(), "seeder"); err == nil {
		t.Fatal("expected unknown --kind to be rejected (the menu of kinds is the ceiling)")
	}
}

// TestSchemaVersionMatchesMigrations keeps extSchemaVersion and the migration
// list in lockstep, so a bumped schema without its migration (or vice versa) is a
// compile-time-adjacent failure rather than a silent gap.
func TestSchemaVersionMatchesMigrations(t *testing.T) {
	if want := len(extMigrations) + 1; extSchemaVersion != want {
		t.Fatalf("extSchemaVersion = %d, but %d migrations imply v%d", extSchemaVersion, len(extMigrations), want)
	}
}

func TestManifestVersion(t *testing.T) {
	cases := []struct{ in, want int }{{0, 1}, {1, 1}, {2, 2}}
	for _, c := range cases {
		if got := manifestVersion(extManifest{SchemaVersion: c.in}); got != c.want {
			t.Errorf("manifestVersion(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestMigrateAddKind(t *testing.T) {
	// internal/ present ⇒ logic-bearing ⇒ check.
	checkDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(checkDir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	m, log, err := migrateAddKind(extManifest{Name: "g"}, checkDir)
	if err != nil || m.Kind != "check" || len(log) != 1 {
		t.Fatalf("internal/ ⇒ check: kind=%q log=%v err=%v", m.Kind, log, err)
	}
	// no logic ⇒ thin tool.
	m, _, _ = migrateAddKind(extManifest{Name: "g"}, t.TempDir())
	if m.Kind != "tool" {
		t.Fatalf("no logic ⇒ tool, got %q", m.Kind)
	}
	// already set ⇒ untouched, no changelog.
	m, log, _ = migrateAddKind(extManifest{Name: "g", Kind: "tool"}, checkDir)
	if m.Kind != "tool" || len(log) != 0 {
		t.Fatalf("preset kind should be left alone: kind=%q log=%v", m.Kind, log)
	}
}

// TestUpgradeV1ToCurrent writes a v1 manifest (no schemaVersion, no kind) and
// asserts upgrade stamps both, and is then idempotent.
func TestUpgradeV1ToCurrent(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "name: legacy\nshort: an old extension\ntools: [renovate]\n")

	if err := runExtensionUpgrade(globalOpts{}, dir, dir, false); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	m := readManifest(t, dir)
	if m.SchemaVersion != extSchemaVersion {
		t.Fatalf("schemaVersion = %d, want %d", m.SchemaVersion, extSchemaVersion)
	}
	if m.Kind != "tool" { // no internal/ or tests ⇒ tool
		t.Fatalf("kind = %q, want tool", m.Kind)
	}
	// Idempotent: a second upgrade is a no-op and --check passes.
	if err := runExtensionUpgrade(globalOpts{}, dir, dir, true); err != nil {
		t.Fatalf("--check on a current extension should pass, got %v", err)
	}
}

func TestUpgradeCheckFailsWhenBehind(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "name: legacy\nshort: old\ntools: [renovate]\n")
	if err := runExtensionUpgrade(globalOpts{}, dir, dir, true); err == nil {
		t.Fatal("--check should exit non-zero on a behind extension")
	}
	// --check must not have written anything.
	if readManifest(t, dir).SchemaVersion != 0 {
		t.Fatal("--check must not mutate recipe.yaml")
	}
}

// TestScaffoldIsBornCurrent: a fresh scaffold needs no upgrade.
func TestScaffoldIsBornCurrent(t *testing.T) {
	dir := t.TempDir()
	if err := runExtensionNew(globalOpts{}, "demo", dir, "check"); err != nil {
		t.Fatal(err)
	}
	if err := runExtensionUpgrade(globalOpts{}, filepath.Join(dir, "demo"), dir, true); err != nil {
		t.Fatalf("freshly-scaffolded extension should be born at the current schema: %v", err)
	}
}

// TestUpgradePreservesComments is the finding-closer: migrating a hand-edited,
// commented v1 manifest must keep the comments + existing keys (copier's hook
// runs this on a just-merged file), while still stamping the new schema.
func TestUpgradePreservesComments(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir,
		"# pin reviewed by security 2026-06\nname: legacy\nshort: an old extension  # keep me\ntools: [renovate]\n")

	if err := runExtensionUpgrade(globalOpts{}, dir, dir, false); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, extensionManifest))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{
		"# pin reviewed by security 2026-06", // head comment survives
		"# keep me",                          // inline comment survives
		"name: legacy",                       // original key untouched
		"kind: tool",                         // migration stamped
		"schemaVersion: 2",                   // schema stamped
	} {
		if !strings.Contains(s, want) {
			t.Errorf("migrated manifest missing %q\n---\n%s", want, s)
		}
	}
}

func TestWiringRendersBothBlocks(t *testing.T) {
	dir := t.TempDir()
	if err := runExtensionNew(globalOpts{}, "renovate", dir, "tool"); err != nil {
		t.Fatal(err)
	}
	copierBlock, err := renderBytes(extensionWiring, "wiring/copier-migrations.yml.tmpl",
		struct {
			Name    string
			Version int
			Dir     string
		}{"renovate", extSchemaVersion, filepath.Join(dir, "renovate")})
	if err != nil {
		t.Fatal(err)
	}
	renovateBlock, err := renderBytes(extensionWiring, "wiring/renovate.json5.tmpl",
		struct {
			Name    string
			Version int
			Dir     string
		}{"renovate", extSchemaVersion, filepath.Join(dir, "renovate")})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"_migrations", "extension", "upgrade"} {
		if !strings.Contains(string(copierBlock), want) {
			t.Errorf("copier block missing %q", want)
		}
	}
	for _, want := range []string{"customManagers", "git-tags", "renovate"} {
		if !strings.Contains(string(renovateBlock), want) {
			t.Errorf("renovate block missing %q", want)
		}
	}
}

func writeManifest(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, extensionManifest), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readManifest(t *testing.T, dir string) extManifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, extensionManifest))
	if err != nil {
		t.Fatal(err)
	}
	var m extManifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
