package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #3 — reconcile is codegen/report, never execution. The ci hook is the only MayMutate
// hook, but its mutation is deferred to workflow runtime: reconcile GENERATES the
// workflow and must never RUN the deploy step. A ci: step that would touch a marker file
// if executed leaves no marker after reconcile — only the generated workflow.
func TestReconcileCIIsCodegenNotExecution(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "DID_DEPLOY")
	manifest := fmt.Sprintf(
		"schemaVersion: 2\nname: wl\nshort: x\nkind: tool\nci:\n  - {name: deploy, anchor: post-converge, argv: [touch, %q]}\n", marker)
	installExt(t, root, "wl", manifest, nil)
	saveExtConfig(root, extConfig{Enabled: []string{"wl"}})

	if err := runExtensionReconcile(globalOpts{yes: true}, root, false); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("reconcile must only GENERATE the ci workflow, never execute the deploy step (MayMutate is deferred to workflow runtime)")
	}
	if _, err := os.Stat(filepath.Join(root, extensionsWorkflowPath)); err != nil {
		t.Fatalf("reconcile should have generated the ci workflow: %v", err)
	}
}

// teardown removes the files an extension owns per the lock and clears its entry; the
// gate (no --yes) removes nothing.
func TestTeardownRemovesOwnedFilesWhenConfirmed(t *testing.T) {
	root := t.TempDir()
	owned := filepath.Join(root, ".github/CODEOWNERS")
	if err := os.MkdirAll(filepath.Dir(owned), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(owned, []byte("* @team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveExtLock(root, extLock{Outputs: map[string][]lockedFile{
		"codeowners": {{Path: ".github/CODEOWNERS", SHA: "x"}},
	}}); err != nil {
		t.Fatal(err)
	}

	// gate: no --yes leaves the file and the lock entry in place
	if err := runExtensionTeardown(globalOpts{}, root, "", false); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := os.Stat(owned); err != nil {
		t.Fatalf("plan must not remove the file: %v", err)
	}

	// --yes removes the file and drops the lock entry
	if err := runExtensionTeardown(globalOpts{yes: true}, root, "", false); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Fatalf("teardown should have removed the file, stat err = %v", err)
	}
	if got := loadExtLock(root).Outputs["codeowners"]; got != nil {
		t.Fatalf("lock entry should be cleared, got %v", got)
	}
}

// teardown [name] scopes to one extension; an unknown name errors.
func TestTeardownScopesByNameAndRejectsUnknown(t *testing.T) {
	root := t.TempDir()
	if err := saveExtLock(root, extLock{Outputs: map[string][]lockedFile{
		"a": {{Path: "a.txt"}}, "b": {{Path: "b.txt"}},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := runExtensionTeardown(globalOpts{yes: true}, root, "a", false); err != nil {
		t.Fatalf("teardown a: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("a.txt should be gone")
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); err != nil {
		t.Fatal("b.txt should be untouched")
	}
	if loadExtLock(root).Outputs["b"] == nil {
		t.Fatal("b's lock entry should survive")
	}
	if err := runExtensionTeardown(globalOpts{yes: true}, root, "nope", false); err == nil {
		t.Fatal("expected an error for an unknown extension")
	}
}

// unseed deletes the GH env secret and prints (never executes) the OpenBao removal;
// the gate (no --yes) deletes nothing.
func TestUnseedDeletesGHAndPrintsBao(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "ohttp",
		"schemaVersion: 2\nname: ohttp\nshort: x\nkind: tool\nsecrets:\n"+
			"  - {name: OHTTP_KEY_SEED, bao: 'secret/ohttp#seed'}\n"+
			"  - {name: FERMYON_CLOUD_TOKEN, ghEnv: infra-lab}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"ohttp"}})

	var deleted [][2]string
	orig := ghDeleteSecretFn
	ghDeleteSecretFn = func(name, env string) error { deleted = append(deleted, [2]string{name, env}); return nil }
	t.Cleanup(func() { ghDeleteSecretFn = orig })

	// gate: no --yes must not delete anything
	if err := runExtensionUnseed(globalOpts{}, root, ""); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("plan must not delete, got %v", deleted)
	}

	// --yes deletes the GH env secret, leaves OpenBao to a printed manual step
	if err := runExtensionUnseed(globalOpts{yes: true}, root, ""); err != nil {
		t.Fatalf("unseed: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != [2]string{"FERMYON_CLOUD_TOKEN", "infra-lab"} {
		t.Fatalf("expected one GH delete of FERMYON_CLOUD_TOKEN/infra-lab, got %v", deleted)
	}
}

// B — the validate tier REQUIRES its tools: a missing tool is a hard failure, not a
// skip (unlike the lint-tier check hook). A present tool that fails also fails.
func TestValidateRequiresTools(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "policy",
		"schemaVersion: 2\nname: policy\nshort: x\nkind: tool\nvalidate:\n  - {name: conftest, argv: [llz-no-such-tool-xyz, test]}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"policy"}})
	if err := runExtensionValidate(globalOpts{}, root); err == nil {
		t.Fatal("a missing required tool must fail validate (not skip like the lint gate)")
	}

	// a present tool that exits non-zero also fails
	installExt(t, root, "policy2",
		"schemaVersion: 2\nname: policy2\nshort: x\nkind: tool\nvalidate:\n  - {name: fail, argv: [sh, -c, 'exit 1']}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"policy2"}})
	if err := runExtensionValidate(globalOpts{}, root); err == nil {
		t.Fatal("a failing validate step must fail")
	}
}

// E — health probes are report-only: a failing probe must NOT propagate an error
// (runExtensionHealth is void and surfaces the failure as a printed signal).
func TestHealthIsReportOnly(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "mon",
		"schemaVersion: 2\nname: mon\nshort: x\nkind: tool\nhealth:\n  - {name: probe, argv: [sh, -c, 'exit 1']}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"mon"}})
	// must not panic and must not block anything — it returns nothing regardless of the
	// probe's exit code. (A failing probe is a signal in doctor/status, not a gate.)
	runExtensionHealth(root)
}

// #2 — dependency-aware teardown: refuse to strip files an enabled extension's
// check/ci hook still consumes (hookDeps); --force overrides. A scaffold-only extension
// (no files-consuming hook) tears down while enabled, proving the check is precise.
func TestTeardownRespectsHookDeps(t *testing.T) {
	root := t.TempDir()
	writeOwned := func(rel string) {
		if err := os.WriteFile(filepath.Join(root, rel), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// lint: enabled + a check hook that consumes the scaffolded .tflintrc.hcl
	installExt(t, root, "lint", "schemaVersion: 2\nname: lint\nshort: x\nkind: tool\ncheck:\n  - {name: tflint, argv: [tflint]}\n", nil)
	// devcontainer: enabled but scaffold-only (no hook consumes its files)
	installExt(t, root, "devcontainer", "schemaVersion: 2\nname: devcontainer\nshort: x\nkind: tool\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"lint", "devcontainer"}})
	saveExtLock(root, extLock{Outputs: map[string][]lockedFile{
		"lint":         {{Path: ".tflintrc.hcl"}},
		"devcontainer": {{Path: ".devcontainer.json"}},
	}})
	writeOwned(".tflintrc.hcl")
	writeOwned(".devcontainer.json")

	// enabled + files-consuming check hook → refuse without --force
	if err := runExtensionTeardown(globalOpts{yes: true}, root, "lint", false); err == nil {
		t.Fatal("teardown must refuse a still-enabled extension whose check hook consumes its files")
	}
	if _, err := os.Stat(filepath.Join(root, ".tflintrc.hcl")); err != nil {
		t.Fatal("the file must survive the refusal")
	}
	// --force overrides
	if err := runExtensionTeardown(globalOpts{yes: true}, root, "lint", true); err != nil {
		t.Fatalf("--force should tear down: %v", err)
	}

	// scaffold-only extension tears down while still enabled (nothing consumes its files)
	if err := runExtensionTeardown(globalOpts{yes: true}, root, "devcontainer", false); err != nil {
		t.Fatalf("scaffold-only extension should tear down while enabled: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".devcontainer.json")); !os.IsNotExist(err) {
		t.Fatal("devcontainer file should be removed")
	}
}

// unseed [name] on an extension that declares no seeded targets errors clearly.
func TestUnseedUnknownOnlyErrors(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "ohttp", "schemaVersion: 2\nname: ohttp\nshort: x\nkind: tool\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"ohttp"}})
	if err := runExtensionUnseed(globalOpts{yes: true}, root, "ghost"); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected a clear error naming ghost, got %v", err)
	}
}
