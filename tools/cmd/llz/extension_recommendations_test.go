package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the four extension-model recommendations implemented in issue #10:
//   1. seed/managed file ownership mode,
//   2. ghVars: (declared GitHub Actions variables) + doctor + seed,
//   3. digest-pin parity for scaffolded workflow files (lintWorkflowImages),
//   4. validate_targets render value (the single-source app CI matrix).

// ── 1. seed mode: write-once, then operator-owned ────────────────────────────

func TestSeedModeWriteOnceAndNotDrift(t *testing.T) {
	manifest := `schemaVersion: 3
name: kit
short: x
kind: tool
stage: app
files:
  - {src: tpl/deny.toml, dst: deny.toml, mode: seed}
  - {src: tpl/ci.yml, dst: .github/workflows/ci.yml}
`
	ext := writeExt(t, manifest, map[string]string{
		"tpl/deny.toml": "[bans]\nmultiple-versions = \"warn\"\n",
		"tpl/ci.yml":    "name: ci\n",
	})
	root := t.TempDir()
	if err := runExtensionApply(globalOpts{}, ext, root, false); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// The operator customizes the seed file.
	seedPath := filepath.Join(root, "deny.toml")
	if err := os.WriteFile(seedPath, []byte("[bans]\nmultiple-versions = \"deny\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Re-apply must NOT clobber the operator's seed edit...
	if err := runExtensionApply(globalOpts{}, ext, root, false); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	got, _ := os.ReadFile(seedPath)
	if !strings.Contains(string(got), `"deny"`) {
		t.Fatalf("seed file was clobbered on re-apply: %q", got)
	}
	// ...and --check must NOT report the edited seed file as drift.
	if err := runExtensionApply(globalOpts{}, ext, root, true); err != nil {
		t.Fatalf("--check should ignore an operator-edited seed file: %v", err)
	}

	// The managed workflow, by contrast, still drifts on a hand-edit.
	if err := os.WriteFile(filepath.Join(root, ".github/workflows/ci.yml"), []byte("name: tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runExtensionApply(globalOpts{}, ext, root, true); err == nil {
		t.Fatal("--check should flag a hand-edited managed file even when a seed file is present")
	}

	// The lock records the ownership class so exclude/teardown still own the seed path.
	var seedLocked bool
	for _, lf := range loadExtLock(root).Outputs["kit"] {
		if lf.Path == "deny.toml" && lf.Mode == "seed" {
			seedLocked = true
		}
	}
	if !seedLocked {
		t.Fatalf("lock should record deny.toml as mode=seed: %+v", loadExtLock(root).Outputs["kit"])
	}
}

func TestFileModeNormalizes(t *testing.T) {
	for in, want := range map[string]string{"": "managed", "managed": "managed", "seed": "seed", "bogus": "managed"} {
		if got := fileMode(in); got != want {
			t.Errorf("fileMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── 2. ghVars: declared GitHub Actions variables ─────────────────────────────

func TestGHVarConfigFindings(t *testing.T) {
	m := extManifest{GHVars: []extGHVar{
		{Name: "RUST_IMAGE", Required: true},          // no default, no override → fatal finding
		{Name: "SPIN_MANIFEST", Default: "spin.toml"}, // default → satisfied
		{Name: "APP_SUFFIX"},                          // no default, optional → non-fatal finding
	}}
	env := func(string) string { return "" }
	findings := manifestConfigFindings("kit", m, env)

	byName := map[string]configFinding{}
	for _, f := range findings {
		byName[f.Name] = f
	}
	if f, ok := byName["RUST_IMAGE"]; !ok || f.Kind != "gh-var" || !f.Fatal {
		t.Fatalf("RUST_IMAGE should be a fatal gh-var finding: %+v", byName)
	}
	if f, ok := byName["APP_SUFFIX"]; !ok || f.Fatal {
		t.Fatalf("APP_SUFFIX should be a non-fatal gh-var finding: %+v", byName)
	}
	if _, ok := byName["SPIN_MANIFEST"]; ok {
		t.Fatalf("SPIN_MANIFEST has a default and should be satisfied: %+v", byName)
	}

	// An LLZ_VAR_<NAME> override satisfies a ghVar with no default.
	env2 := func(k string) string {
		if k == varOverrideEnv("RUST_IMAGE") {
			return "ghcr.io/x@sha256:abc"
		}
		return ""
	}
	for _, f := range manifestConfigFindings("kit", m, env2) {
		if f.Name == "RUST_IMAGE" {
			t.Fatalf("RUST_IMAGE override should satisfy the finding, got %+v", f)
		}
	}
}

func TestSeedPushesGHVars(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "kit", "schemaVersion: 3\nname: kit\nshort: x\nkind: tool\nstage: app\nghVars:\n"+
		"  - {name: RUST_IMAGE, default: 'ghcr.io/x@sha256:abc'}\n"+ // repo-level
		"  - {name: SPIN_MANIFEST, ghEnv: infra-prod, default: spin.toml}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"kit"}})

	var got [][]string
	orig := seedGHVarFn
	seedGHVarFn = func(_ globalOpts, env, name, value string) error {
		got = append(got, []string{env, name, value})
		return nil
	}
	t.Cleanup(func() { seedGHVarFn = orig })

	if err := runExtensionSeed(globalOpts{yes: true}, root); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"RUST_IMAGE": "ghcr.io/x@sha256:abc", "SPIN_MANIFEST": "spin.toml"}
	if len(got) != 2 {
		t.Fatalf("expected 2 gh-var writes, got %v", got)
	}
	for _, w := range got {
		if want[w[1]] != w[2] {
			t.Fatalf("gh-var %s pushed %q, want %q", w[1], w[2], want[w[1]])
		}
	}
}

// ── 3. digest-pin parity for scaffolded workflow files ───────────────────────

func TestLintWorkflowImages(t *testing.T) {
	const wfManifest = `schemaVersion: 3
name: kit
short: x
kind: tool
stage: app
ghVars:
  - {name: RUST_IMAGE}
files:
  - {src: wf, dst: .github/workflows}
`
	cases := []struct {
		name      string
		image     string
		wantClean bool
	}{
		{"declared vars ref", "${{ vars.RUST_IMAGE }}", true},
		{"digest pinned", "ghcr.io/o/img@sha256:" + strings.Repeat("a", 64), true},
		{"undeclared vars ref", "${{ vars.NOPE }}", false},
		{"mutable tag", "ghcr.io/o/img:latest", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ext, err := extensionFromDir(writeExt(t, wfManifest, map[string]string{
				"wf/ci.yml": "jobs:\n  q:\n    container:\n      image: " + tc.image + "\n",
			}))
			if err != nil {
				t.Fatal(err)
			}
			findings := lintWorkflowImages(ext)
			if tc.wantClean && len(findings) != 0 {
				t.Fatalf("expected clean, got %v", findings)
			}
			if !tc.wantClean && len(findings) == 0 {
				t.Fatalf("expected a finding for image %q", tc.image)
			}
		})
	}
}

// A non-workflow file is never image-checked, even if it mentions a mutable image.
func TestLintWorkflowImagesIgnoresNonWorkflow(t *testing.T) {
	ext, err := extensionFromDir(writeExt(t, "schemaVersion: 3\nname: kit\nshort: x\nkind: tool\nstage: app\nfiles:\n  - {src: notes.md, dst: docs/notes.md}\n",
		map[string]string{"notes.md": "use image: ghcr.io/x:latest in prod\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if f := lintWorkflowImages(ext); len(f) != 0 {
		t.Fatalf("non-workflow file must not be image-checked, got %v", f)
	}
}

// ── 4. validate_targets: single-source app CI matrix ─────────────────────────

func TestValidateTargets(t *testing.T) {
	m := extManifest{Validate: []extStep{
		{Name: "fmt-check", Argv: []string{"q", "fmt-check"}},
		{Name: "clippy", Argv: []string{"q", "clippy"}},
		{Argv: []string{"q", "noname"}}, // no name → no matrix identity → skipped
	}}
	if got := validateTargets(m); got != "[fmt-check, clippy]" {
		t.Fatalf("validateTargets = %q, want [fmt-check, clippy]", got)
	}
	if got := validateTargets(extManifest{}); got != "[]" {
		t.Fatalf("empty validate: should render [], got %q", got)
	}
}

// A scaffolded workflow renders its matrix from the manifest's validate: names, so the
// list lives in exactly one place.
func TestValidateTargetsRendersIntoWorkflow(t *testing.T) {
	manifest := `schemaVersion: 3
name: kit
short: x
kind: app
stage: app
validate:
  - {name: fmt-check, argv: [scripts/q.sh, fmt-check]}
  - {name: clippy, argv: [scripts/q.sh, clippy]}
files:
  - {src: ci.yml, dst: .github/workflows/ci.yml}
`
	ext := writeExt(t, manifest, map[string]string{"ci.yml": "    target: <@ .validate_targets @>\n"})
	root := t.TempDir()
	if err := runExtensionApply(globalOpts{}, ext, root, false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, ".github/workflows/ci.yml"))
	if !strings.Contains(string(got), "target: [fmt-check, clippy]") {
		t.Fatalf("workflow matrix should render from validate:, got %q", got)
	}
}
