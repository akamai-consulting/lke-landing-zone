package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Adoption: a file present on disk, matching an available built-in's output, not yet in the
// lock (evidence it migrated out of the template) → the extension is enabled + recorded,
// without overwriting the present file.
func TestAdoptDetectsMigratedFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".yamllint.yaml"), []byte("rules: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		if err := runExtensionAdopt(globalOpts{}, root); err != nil {
			t.Fatalf("adopt: %v", err)
		}
	})
	cfg, _ := loadExtConfig(root)
	if !slices.Contains(cfg.Enabled, "lint-yaml") {
		t.Fatalf("lint-yaml should be adopted/enabled, got %v", cfg.Enabled)
	}
	if _, ok := loadExtLock(root).Outputs["lint-yaml"]; !ok {
		t.Fatal("the adopted file should be recorded in the lock")
	}
	if got, _ := os.ReadFile(filepath.Join(root, ".yamllint.yaml")); string(got) != "rules: {}\n" {
		t.Fatalf("adoption must not overwrite the present file, got %q", got)
	}
}

// The adopted path must land in ownedPaths — that is the exact set runUpgrade excludes from
// `copier update`, so the migrated file survives the update instead of being deleted.
func TestAdoptedPathFencesCopier(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".yamllint.yaml"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() { _ = runExtensionAdopt(globalOpts{}, root) })
	if !slices.Contains(ownedPaths(loadExtLock(root)), ".yamllint.yaml") {
		t.Fatal("adopted path must be in ownedPaths so `copier update` excludes (keeps) it")
	}
}

// A deliberately DISABLED extension keeps its lock entry, so adoption must NOT re-enable it
// (the lock discriminates a migrated-in file from one we applied then the operator disabled).
func TestAdoptSkipsDeliberatelyDisabled(t *testing.T) {
	root := t.TempDir()
	captureStdout(t, func() {
		_ = runExtensionEnable(globalOpts{}, root, "lint-yaml")
		_ = runExtensionDisable(globalOpts{}, root, "lint-yaml")
		if err := runExtensionAdopt(globalOpts{}, root); err != nil {
			t.Fatalf("adopt: %v", err)
		}
	})
	if cfg, _ := loadExtConfig(root); slices.Contains(cfg.Enabled, "lint-yaml") {
		t.Fatal("a deliberately-disabled extension must not be re-adopted")
	}
}

func TestAdoptDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".yamllint.yaml"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() { _ = runExtensionAdopt(globalOpts{dryRun: true}, root) })
	if cfg, _ := loadExtConfig(root); slices.Contains(cfg.Enabled, "lint-yaml") {
		t.Fatal("dry-run adopt must not enable")
	}
	if _, ok := loadExtLock(root).Outputs["lint-yaml"]; ok {
		t.Fatal("dry-run adopt must not record the lock")
	}
}

// These tests pin the three integration fixes surfaced by driving a built-in candidate
// (lint-yaml) end-to-end through enable → apply → doctor → drift (issue #10).

// Fix A: enabling an optional built-in applies the FULL enabled + always-on set, so an
// always-on built-in's files (gitattributes) land too and the instance is immediately
// drift-clean — not flagged by the next `apply --check`.
func TestEnableLeavesAlwaysOnApplied(t *testing.T) {
	root := t.TempDir()
	captureStdout(t, func() {
		if err := runExtensionEnable(globalOpts{}, root, "lint-yaml"); err != nil {
			t.Fatalf("enable: %v", err)
		}
	})
	if _, err := os.Stat(filepath.Join(root, ".gitattributes")); err != nil {
		t.Fatalf("enable should also apply the always-on gitattributes built-in: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".yamllint.yaml")); err != nil {
		t.Fatalf("enable should apply the enabled extension's file: %v", err)
	}
	// The whole set is consistent → apply --check is clean right after enable.
	if err := runExtensionApplyAll(globalOpts{}, root, true); err != nil {
		t.Fatalf("apply --check should be clean immediately after enable: %v", err)
	}
}

// Fix A (dry-run preserved): a dry-run enable previews only the target and writes nothing —
// it does not apply the always-on set.
func TestEnableDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	captureStdout(t, func() { _ = runExtensionEnable(globalOpts{dryRun: true}, root, "lint-yaml") })
	if _, err := os.Stat(filepath.Join(root, ".gitattributes")); !os.IsNotExist(err) {
		t.Fatal("dry-run enable must not apply the always-on built-in")
	}
	if _, err := os.Stat(filepath.Join(root, ".yamllint.yaml")); !os.IsNotExist(err) {
		t.Fatal("dry-run enable must not write the target file")
	}
}

// Fix B: the standalone `extension doctor` reports a missing declared tool (not only core
// `llz doctor`), so it is a complete Configure-readiness check.
func TestExtensionDoctorReportsMissingTool(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "needstool", "schemaVersion: 3\nname: needstool\nshort: x\nkind: tool\nstage: universal\n"+
		"tools:\n  - {name: llz-absent-tool-xyz}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"needstool"}})
	out := captureStdout(t, func() { _ = runExtensionConfigDoctor(root) })
	if !strings.Contains(out, "llz-absent-tool-xyz") {
		t.Fatalf("extension doctor should report the missing tool, got: %q", out)
	}
}

// Fix C: `llz drift` reports extension output drift even when there is no .template-version
// (extensions are a separate delivery channel from the copier template), before the
// template-version error.
func TestDriftReportsExtensionDriftWithoutTemplateVersion(t *testing.T) {
	root := t.TempDir()
	captureStdout(t, func() { _ = runExtensionEnable(globalOpts{}, root, "lint-yaml") })
	if err := os.WriteFile(filepath.Join(root, ".yamllint.yaml"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	out := captureStdout(t, func() { _ = runDrift("main", "", false) }) // returns the .template-version error
	if !strings.Contains(out, "Extension drift") || !strings.Contains(out, "lint-yaml") {
		t.Fatalf("drift should report extension output drift before the template error, got: %q", out)
	}
}
