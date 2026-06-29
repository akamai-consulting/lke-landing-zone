package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
