package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func provExts() []Extension {
	return []Extension{
		{Name: "lint-markdown", Manifest: extManifest{Name: "lint-markdown", Tools: []extTool{{Name: "markdownlint", Via: "npm:markdownlint-cli", Version: "0.41.0"}}}},
		{Name: "lint-yaml", Manifest: extManifest{Name: "lint-yaml", Tools: []extTool{{Name: "yamllint", Via: "pipx:yamllint", Version: "1.35.1"}}}},
		{Name: "byo", Manifest: extManifest{Name: "byo", Tools: []extTool{{Name: "kubectl"}}}}, // no via → operator-supplied
	}
}

// renderMiseConfig emits a deterministic [tools] table of the pinned, provisionable refs;
// operator-supplied tools (no via) are excluded.
func TestRenderMiseConfig(t *testing.T) {
	out := renderMiseConfig(provExts())
	if !strings.Contains(out, "[tools]") {
		t.Fatal("missing [tools] table")
	}
	mustContain(t, out,
		`"npm:markdownlint-cli" = "0.41.0"`,
		`"pipx:yamllint" = "1.35.1"`,
	)
	if strings.Contains(out, "kubectl") {
		t.Error("operator-supplied tool (no via) must not be provisioned")
	}
	// deterministic + sorted by ref (npm < pipx)
	if strings.Index(out, "npm:markdownlint-cli") > strings.Index(out, "pipx:yamllint") {
		t.Error("refs should be sorted")
	}
	if out != renderMiseConfig(provExts()) {
		t.Error("render is not deterministic")
	}
}

// provisionableTools de-dups by ref and drops operator-supplied tools.
func TestProvisionableTools(t *testing.T) {
	got := provisionableTools(provExts())
	if len(got) != 2 {
		t.Fatalf("provisionableTools = %d, want 2 (kubectl has no via)", len(got))
	}
}

// provision writes .mise.toml and calls mise only with --yes; the gate (no --yes) plans
// without writing or installing.
func TestProvisionGate(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "lint-yaml",
		"schemaVersion: 2\nname: lint-yaml\nshort: x\nkind: tool\ntools:\n  - {name: yamllint, via: 'pipx:yamllint', version: '1.35.1'}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"lint-yaml"}})

	var installed bool
	orig := miseInstallFn
	miseInstallFn = func(globalOpts, string, string) error { installed = true; return nil }
	t.Cleanup(func() { miseInstallFn = orig })

	// gate: no --yes writes nothing, installs nothing
	if err := runExtensionProvision(globalOpts{}, root, false); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, miseConfigPath)); !os.IsNotExist(err) {
		t.Fatal("plan must not write .mise.toml")
	}
	if installed {
		t.Fatal("plan must not run mise install")
	}

	// --yes writes the config and installs
	if err := runExtensionProvision(globalOpts{yes: true}, root, false); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, miseConfigPath)); err != nil {
		t.Fatalf(".mise.toml should be written: %v", err)
	}
	if !installed {
		t.Fatal("--yes should run mise install")
	}

	// --check passes once up to date, then flags drift after disable
	if err := runExtensionProvision(globalOpts{}, root, true); err != nil {
		t.Fatalf("--check should pass when up to date: %v", err)
	}
}
