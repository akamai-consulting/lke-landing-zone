package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"sigs.k8s.io/yaml"
)

// extension.go is an EXPERIMENT (issue #10): the scaffolder-first take on the
// recipe/extension vehicle. The thesis is that the relief valve for core bloat is
// not a registry but a *gradient* — `llz extension new` must make a well-formed,
// testable, ceiling-respecting extension cheaper to create than a new ci_*.go in
// core, so the path of least resistance points OUT of the binary. Two commands:
//
//	llz extension new <name> --kind check|tool
//	    Clone an embedded skeleton (the same trick scaffold.go uses to clone the
//	    `example` overlay) into <dir>/<name>, rendered for <name>. The *menu of
//	    kinds is the capability ceiling*: there is a `check` and a `tool` skeleton
//	    and deliberately no `seeder`/`bootstrap-step` — if there's no skeleton to
//	    start from, that capability belongs in core, not an extension.
//
//	llz extension lint <dir>
//	    The boundary-side reincarnation of `ci untestable-loc`: the manifest must
//	    be argv-only (no inline shell smuggling untested logic past the gate), and
//	    a logic-bearing (kind: check) extension must ship tests. The principle
//	    "logic must be unit-tested" thus TRAVELS with the extension instead of
//	    being enforced in core. The skeleton wires this lint into its own `check:`,
//	    so every extension validates itself against the ceiling.
//
// Out of scope for the experiment (on purpose): the loader/registry, git fetch,
// and the .llz/recipes.lock test attestation. We want to learn whether the
// skeleton is the right SHAPE first; the runtime is just "load and run it".

//go:embed all:extensions
var extensionSkeletons embed.FS

//go:embed all:wiring
var extensionWiring embed.FS

const extensionManifest = "recipe.yaml"

// extManifest is the declarative projection a built-in or remote extension loads
// into — the subset the experiment needs. `kind` selects the ceiling rule lint
// applies.
type extManifest struct {
	// SchemaVersion is the manifest schema this extension was authored against.
	// Absent/0 means v1 (predates the field). `llz extension upgrade` migrates it
	// up to extSchemaVersion, the current schema baked into this binary.
	SchemaVersion int          `json:"schemaVersion,omitempty"`
	Name          string       `json:"name"`
	Short         string       `json:"short"`
	Kind          string       `json:"kind"`               // "check" (logic-bearing, ships tests) | "tool" (thin argv wrap)
	Stage         Stage        `json:"stage,omitempty"`    // delivery layer: iac | kube-infra | app (empty = cross-cutting). App checks gate in the app's CI, not the platform gate.
	Optional      bool         `json:"optional,omitempty"` // built-ins only: ships with the binary but OFF by default (opt-in via `llz extension enable`)
	Tools         []extTool    `json:"tools,omitempty"`    // external tools the steps need; doctor verifies, `llz extension provision` installs (via mise)
	Vars          []extVar     `json:"vars,omitempty"`     // Configure phase: declared template inputs
	Secrets       []extSecret  `json:"secrets,omitempty"`  // Configure phase: declared runtime secrets
	Files         []extFile    `json:"files,omitempty"`    // Scaffold phase: rendered into the instance
	Check         []extStep    `json:"check,omitempty"`    // Gate phase (lint tier): folded into runLint; missing tool skips
	Validate      []extStep    `json:"validate,omitempty"` // Gate phase (CI tier): folded into runValidate; tools REQUIRED
	CI            []extStep    `json:"ci,omitempty"`       // Bootstrap phase: the workflow DAG
	Health        []extStep    `json:"health,omitempty"`   // Operate phase: report-only probes surfaced by doctor/status
	Commands      []extCommand `json:"commands,omitempty"` // Operate phase: operator CLI (reuses ext.go's extCommand)
	Rotate        *extRotate   `json:"rotate,omitempty"`   // Operate/Sustain: implements the TokenRotator interface
}

// extTool is an external tool a step invokes. Name is the executable on PATH (what
// doctor verifies). Via + Version are the DECLARATIVE provisioning spec consumed by
// `llz extension provision`: Via is a mise backend ref (e.g. "pipx:yamllint",
// "npm:markdownlint-cli", "aqua:crate-ci/typos") and Version pins it. Crucially the
// extension declares WHAT to install (a pinned, registry-resolvable ref) — never HOW:
// there is no install-script field, so a remote extension cannot smuggle host execution.
// An empty Via means "operator-supplied" (declared + verified, but not auto-provisioned).
type extTool struct {
	Name    string `json:"name"`
	Via     string `json:"via,omitempty"`
	Version string `json:"version,omitempty"`
}

// UnmarshalJSON accepts either a bare string — `tools: [yamllint]`, the shorthand for an
// operator-supplied tool with no provisioning spec — or the full object form
// `{name, via, version}`. Keeping the string shorthand makes the structured-Tools change
// backward-compatible with every pre-existing manifest (and sigs.k8s.io/yaml routes YAML
// through encoding/json, so this covers YAML too).
func (t *extTool) UnmarshalJSON(b []byte) error {
	var name string
	if err := json.Unmarshal(b, &name); err == nil {
		t.Name, t.Via, t.Version = name, "", ""
		return nil
	}
	type raw extTool // shed the custom unmarshaler to avoid recursion
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	*t = extTool(r)
	return nil
}

// extRotate is the declarative side of the TokenRotator interface: Argv mints a
// fresh token and prints its new value to stdout; Secret names the declared
// secret: entry to re-seed with it (so rotation reuses the seed targets).
type extRotate struct {
	Argv   []string `json:"argv"`
	Secret string   `json:"secret"`
}

// findSecret returns the declared secret with the given name.
func findSecret(m extManifest, name string) (extSecret, bool) {
	for _, s := range m.Secrets {
		if s.Name == name {
			return s, true
		}
	}
	return extSecret{}, false
}

// extFile maps a source path within the extension to a destination in the
// instance repo. The body is rendered through the same <@ @> engine (copier's
// variable delimiters) the skeleton scaffolder uses, so built-in and remote files
// render identically — and identically to copier for the variable-substitution case.
//
// Src may be a FILE or a DIRECTORY. A directory scaffolds the whole subtree: each file
// renders to Dst joined with its path relative to Src — so a workload kit can ship a
// Cargo workspace (or any tree) without hand-listing every file. It flattens to per-file
// outputs, so the lock, --check drift, exclude, and teardown all treat it like any other
// scaffolded file. Dst is NOT templated at apply time (only file bodies are); it is fixed
// when the extension is authored (`extension new` renders it).
type extFile struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
}

// extVar is a declared template input. Its Default feeds the Scaffold render
// (overridable by LLZ_VAR_<NAME>); a var with no default is surfaced by doctor.
type extVar struct {
	Name    string `json:"name"`
	Default string `json:"default,omitempty"`
	Doc     string `json:"doc,omitempty"`
}

// extSecret is a declared runtime secret. doctor checks it is present as an env
// var (the TF_VAR_*/CI model); a missing Required secret is a hard finding. The
// optional Bao/GHEnv targets let `llz extension seed` WIRE the env value into a
// store; a secret with no target stays declare-only. The value is never stored in
// the manifest or repo — only read from the environment at seed time.
type extSecret struct {
	Name     string `json:"name"`
	Doc      string `json:"doc,omitempty"`
	Required bool   `json:"required,omitempty"`
	Bao      string `json:"bao,omitempty"`   // OpenBao target "path#key"
	GHEnv    string `json:"ghEnv,omitempty"` // GitHub Environment to set <Name> in
}

// extSchemaVersion is the current manifest schema this binary speaks. Bump it
// when a skeleton/manifest convention changes and append the migration that
// carries older extensions forward.
const extSchemaVersion = 2

// manifestVersion is m's schema version, defaulting absent/0 to v1 (the schema
// before SchemaVersion existed).
func manifestVersion(m extManifest) int {
	if m.SchemaVersion < 1 {
		return 1
	}
	return m.SchemaVersion
}

// extStep is one gate step. Argv is run verbatim against a tool the operator
// already has — the same model as .llz/commands.yaml (ext.go), but lint-gated so
// it can't carry inline logic. For ci: steps, Anchor binds the step to a
// lifecycle position and DependsOn names other extension steps it must follow —
// both consumed by the workflow codegen (extension_ci.go), not by lint.
type extStep struct {
	Name      string   `json:"name,omitempty"`
	Anchor    string   `json:"anchor,omitempty"`   // ci: only — pre-converge | post-converge | operate
	Schedule  string   `json:"schedule,omitempty"` // ci: only — a cron expr makes this a scheduled (vs converge-anchored) job
	Image     string   `json:"image,omitempty"`    // ci: only — a digest-pinned image the job runs in (the CI tool-supply: container: ...)
	Argv      []string `json:"argv"`
	DependsOn []string `json:"dependsOn,omitempty"` // ci: only — other "ext:step" ids
}

// manifestDeclaresHook reports whether manifest m declares anything for hook kind k —
// the bridge from the lifecycle hook taxonomy to the manifest sections (used by
// dependency-aware teardown to ask "does this extension have a files-dependent hook?").
func manifestDeclaresHook(m extManifest, k HookKind) bool {
	switch k {
	case HookFiles:
		return len(m.Files) > 0
	case HookConfig:
		return len(m.Vars) > 0 || len(m.Secrets) > 0
	case HookCheck:
		return len(m.Check) > 0
	case HookValidate:
		return len(m.Validate) > 0
	case HookCI:
		return len(m.CI) > 0
	case HookHealth:
		return len(m.Health) > 0
	case HookCommands:
		return len(m.Commands) > 0
	}
	return false
}

func allSteps(m extManifest) []extStep {
	out := append([]extStep{}, m.Check...)
	out = append(out, m.Validate...)
	out = append(out, m.CI...)
	out = append(out, m.Health...)
	return out
}

// ── lint: the capability ceiling ─────────────────────────────────────────────

// shells are the interpreters that, with -c, turn an argv step back into inline,
// untested logic — exactly what `ci untestable-loc` exists to ratchet out. An
// extension that smuggles a script through `argv: [bash, -c, "…20 lines…"]` has
// only moved the untestable surface from core's workflows to its own. Reject it.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

// isInlineShell reports whether argv is an interpreter invoked with -c, i.e. the
// step carries inline script instead of calling a named, testable entrypoint.
func isInlineShell(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	if !shells[filepath.Base(argv[0])] {
		return false
	}
	for _, a := range argv[1:] {
		if a == "-c" {
			return true
		}
	}
	return false
}

// lintManifest applies the origin-agnostic structural rules: required identity, a
// known kind, and the argv-only ceiling on every step. Pure (no I/O) and table-
// tested; returns one finding per problem (empty slice == clean).
func lintManifest(m extManifest) []string {
	var f []string
	if strings.TrimSpace(m.Name) == "" {
		f = append(f, "name: is required")
	}
	if strings.TrimSpace(m.Short) == "" {
		f = append(f, "short: is required")
	}
	switch m.Kind {
	case "check", "tool":
	case "":
		f = append(f, "kind: is required (check | tool)")
	default:
		f = append(f, fmt.Sprintf("kind: %q is not one of check|tool", m.Kind))
	}
	if m.Stage != "" {
		if _, ok := stageMeta(m.Stage); !ok {
			f = append(f, fmt.Sprintf("stage: %q is not one of iac|kube-infra|app", m.Stage))
		}
	}
	for i, s := range allSteps(m) {
		label := s.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i)
		}
		switch {
		case len(s.Argv) == 0:
			f = append(f, fmt.Sprintf("step %s: argv is empty", label))
		case isInlineShell(s.Argv):
			f = append(f, fmt.Sprintf("step %s: inline shell (`%s -c …`) is rejected — call a tested entrypoint, not inline script",
				label, filepath.Base(s.Argv[0])))
		}
	}
	// A ci: step's container image (the CI tool-supply) must be digest-pinned — a remote
	// image runs with the workflow's permissions, so a mutable tag is trust surface.
	for i, s := range m.CI {
		label := s.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i)
		}
		if err := validateCIImage("ci step "+label, s.Image); err != nil {
			f = append(f, err.Error())
		}
	}
	// Configure-phase declarations need a name to be checkable.
	for i, v := range m.Vars {
		if v.Name == "" {
			f = append(f, fmt.Sprintf("var #%d: name is required", i))
		}
	}
	for i, s := range m.Secrets {
		if s.Name == "" {
			f = append(f, fmt.Sprintf("secret #%d: name is required", i))
		}
	}
	// A rotate: block (TokenRotator) is argv-only and must refresh a declared,
	// targeted secret — otherwise the rotation has nowhere to land.
	if r := m.Rotate; r != nil {
		switch {
		case len(r.Argv) == 0:
			f = append(f, "rotate: argv is empty")
		case isInlineShell(r.Argv):
			f = append(f, fmt.Sprintf("rotate: inline shell (`%s -c …`) is rejected — call a tested entrypoint", filepath.Base(r.Argv[0])))
		}
		switch {
		case r.Secret == "":
			f = append(f, "rotate: secret is required (which secret it refreshes)")
		default:
			if s, ok := findSecret(m, r.Secret); !ok {
				f = append(f, fmt.Sprintf("rotate: secret %q is not declared in secrets:", r.Secret))
			} else if s.Bao == "" && s.GHEnv == "" {
				f = append(f, fmt.Sprintf("rotate: secret %q has no bao:/ghEnv: target — the new token has nowhere to land", r.Secret))
			}
		}
	}
	// Operate-phase commands carry the SAME argv-only ceiling — a `commands:` entry
	// can't smuggle inline shell any more than a check/ci step can.
	for i, c := range m.Commands {
		switch {
		case c.Name == "":
			f = append(f, fmt.Sprintf("command #%d: name is required", i))
		case len(c.Argv) == 0:
			f = append(f, fmt.Sprintf("command %q: argv is empty", c.Name))
		case isInlineShell(c.Argv):
			f = append(f, fmt.Sprintf("command %q: inline shell (`%s -c …`) is rejected — call a tested entrypoint",
				c.Name, filepath.Base(c.Argv[0])))
		}
	}
	return f
}

// lintKind applies the ceiling rule that needs filesystem signal: a logic-bearing
// (kind: check) extension must ship tests, so "logic is unit-tested" travels with
// the extension rather than being enforced in core. hasTests is whether the tree
// carries any *_test.go — the experiment's proxy for a real, green test suite
// (the production gate is the lock recording the pinned SHA's CI as passing).
func lintKind(m extManifest, hasTests bool) []string {
	if m.Kind == "check" && !hasTests {
		return []string{"kind: check is logic-bearing but ships no *_test.go — the rule that logic must be unit-tested travels with the extension (cf. `llz ci untestable-loc`)"}
	}
	return nil
}

// ── upgrade: migrate to the current schema ───────────────────────────────────

// extMigration upgrades a manifest (and, if needed, its tree at dir) from schema
// version v to v+1, returning a human-readable changelog. extMigrations[i] is the
// v=(i+1) → v+2 step, so the current schema is len(extMigrations)+1 — kept in
// sync with extSchemaVersion by TestSchemaVersionMatchesMigrations.

func runExtensionLint(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, extensionManifest))
	if err != nil {
		return fmt.Errorf("read %s: %w", extensionManifest, err)
	}
	var m extManifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("%s: %w", extensionManifest, err)
	}
	findings := append(lintManifest(m), lintKind(m, hasGoTests(dir))...)
	if len(findings) == 0 {
		fmt.Fprintf(os.Stderr, "extension %q: ok\n", m.Name)
		return nil
	}
	fmt.Fprintf(os.Stderr, "extension %q: %d problem(s):\n", m.Name, len(findings))
	for _, x := range findings {
		fmt.Fprintf(os.Stderr, "  • %s\n", x)
	}
	return fmt.Errorf("extension lint failed")
}

// hasGoTests reports whether dir carries any *_test.go (recursively).
func hasGoTests(dir string) bool {
	var found bool
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), "_test.go") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// extScaffoldData is the render context handed to every skeleton template. Delims
// are copier's <@ @> (from copier.yml _envops), so extensions and copier share ONE
// authoring syntax — variable substitution renders byte-identically to copier; the
// engine is Go text/template (simple substitution), NOT full Python Jinja2, so
// blocks/filters are a deliberate non-feature (keep extension templates simple).
// The <@ @> delimiters also dodge literal ${{ … }} Actions expressions and {{ }} Go.
type extScaffoldData struct {
	Name string
	Kind string
	Tool string // kind=tool: the external tool the extension wraps (default: Name)
}

func runExtensionNew(g globalOpts, name, dir, kind string) error {
	switch kind {
	case "check", "tool", "observability":
	default:
		return fmt.Errorf("unknown --kind %q (choose check | tool | observability); the menu of kinds IS the capability ceiling", kind)
	}
	dst := filepath.Join(dir, name)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s already exists", dst)
	}
	root := "extensions/skeleton-" + kind
	data := extScaffoldData{Name: name, Kind: kind, Tool: name}

	err := fs.WalkDir(extensionSkeletons, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		outRel := strings.TrimSuffix(rel, ".tmpl") // skeleton ships *.go.tmpl so the parent module never compiles it
		out, rerr := renderBytes(extensionSkeletons, p, data)
		if rerr != nil {
			return fmt.Errorf("render %s: %w", rel, rerr)
		}
		outPath := filepath.Join(dst, outRel)
		if g.dryRun {
			fmt.Fprintf(os.Stderr, "→ (dry-run) would write %s\n", outPath)
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if filepath.Base(outPath) == "check" {
			mode = 0o755 // the entrypoint is executable
		}
		return os.WriteFile(outPath, out, mode)
	})
	if err != nil {
		return err
	}
	if !g.dryRun {
		fmt.Fprintf(os.Stderr, "scaffolded %s (kind: %s)\n", dst, kind)
		if kind == "check" { // only the check skeleton ships Go logic + tests
			fmt.Fprintf(os.Stderr, "next: (cd %s && go test ./...) && llz extension lint %s\n", dst, dst)
		} else {
			fmt.Fprintf(os.Stderr, "next: edit the scaffold, then llz extension lint %s\n", dst)
		}
	}
	return nil
}

func renderBytes(fsys fs.FS, path string, data any) ([]byte, error) {
	raw, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, err
	}
	t, err := template.New(filepath.Base(path)).Delims("<@", "@>").Parse(string(raw))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// runExtensionWiring prints the reuse-path glue for a REMOTE extension: a copier
// `migrations:` block (copier sequences the update + 3-way merge, then calls the
// binary's tested `extension upgrade`) and a renovate custom manager (opens a PR
// when the pinned upstream tag moves). Nothing here is new machinery — it wires
// existing tools to the parts they're each best at.
func runExtensionWiring(dir string) error {
	name := filepath.Base(filepath.Clean(dir))
	if b, err := os.ReadFile(filepath.Join(dir, extensionManifest)); err == nil {
		var m extManifest
		if yaml.Unmarshal(b, &m) == nil && m.Name != "" {
			name = m.Name
		}
	}
	data := struct {
		Name    string
		Version int
		Dir     string
	}{name, extSchemaVersion, dir}

	for _, w := range []struct{ banner, path string }{
		{"copier.yml — in the REMOTE extension template; runs the migration on `copier update`:", "wiring/copier-migrations.yml.tmpl"},
		{"renovate.json5 — in the CONSUMING instance; PRs the pinned ref forward when upstream tags:", "wiring/renovate.json5.tmpl"},
	} {
		out, err := renderBytes(extensionWiring, w.path, data)
		if err != nil {
			return err
		}
		fmt.Printf("# ── %s\n\n%s\n", w.banner, out)
	}
	return nil
}
