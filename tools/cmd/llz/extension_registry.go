package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"text/tabwriter"
)

// builtinExtensionsFn is a seam so tests can inject a built-in set (e.g. an optional
// built-in) without touching the embed.
var builtinExtensionsFn = builtinExtensions

// optionalBuiltin returns the compiled-in built-in named name IF it ships off-by-default
// (Optional). These are the third tier: shipped with the binary, but opt-in like a
// local/remote extension — the home an always-on built-in (gitattributes) and an
// instance-local extension leave between them.
func optionalBuiltin(name string) (Extension, bool) {
	for _, b := range builtinExtensionsFn() {
		if b.Name == name && b.Manifest.Optional {
			return b, true
		}
	}
	return Extension{}, false
}

// loadEnabledExtensions is the opt-in set: every name in .llz/extensions.yaml, resolved
// to an enabled OPTIONAL built-in (from the embed), else a local extensions/<name>/, else
// a synced source. Always-on built-ins are NOT here (they come from loadAllExtensions).
func loadEnabledExtensions(root string) ([]Extension, error) {
	cfg, err := loadExtConfig(root)
	if err != nil {
		return nil, err
	}
	if err := verifyRemoteCache(root); err != nil { // integrity-check fetched sources before reading them
		return nil, err
	}
	builtinName := map[string]bool{}
	for _, b := range builtinExtensionsFn() {
		builtinName[b.Name] = true
	}
	var out []Extension
	for _, name := range cfg.Enabled {
		if b, ok := optionalBuiltin(name); ok { // a shipped, off-by-default built-in
			out = append(out, b)
			continue
		}
		if builtinName[name] {
			continue // an always-on built-in redundantly listed → loadAllExtensions owns it
		}
		dir, ok := resolveExtensionDir(root, cfg, name)
		if !ok {
			return nil, fmt.Errorf("enabled extension %q: not found locally or in any synced source (run `llz extension sync`)", name)
		}
		m, err := readManifestAt(dir)
		if err != nil {
			return nil, fmt.Errorf("enabled extension %q: %w", name, err)
		}
		source := "local"
		if rs, ok := remoteSourceOf(root, cfg, name); ok {
			source = rs.Repo + "@" + rs.Ref
		}
		out = append(out, Extension{Name: name, Source: source, Dir: dir, fsys: os.DirFS(dir), Manifest: m})
	}
	return out, nil
}

// loadAllExtensions is the lifecycle view: ALWAYS-ON built-ins plus the enabled set
// (which already includes any enabled optional built-ins). Built-ins come first so they
// reconcile before operator extensions.
func loadAllExtensions(root string) ([]Extension, error) {
	enabled, err := loadEnabledExtensions(root)
	if err != nil {
		return nil, err
	}
	var alwaysOn []Extension
	for _, b := range builtinExtensionsFn() {
		if !b.Manifest.Optional {
			alwaysOn = append(alwaysOn, b)
		}
	}
	return append(alwaysOn, enabled...), nil
}

// resolveExtensionDir finds an enabled extension: local extensions/<name>/ first,
// then each synced source's <name>/ subdir. First match wins.
func resolveExtensionDir(root string, cfg extConfig, name string) (string, bool) {
	local := filepath.Join(root, cfg.extDir(), name)
	if _, err := os.Stat(filepath.Join(local, extensionManifest)); err == nil {
		return local, true
	}
	src, ok := remoteSourceOf(root, cfg, name)
	if !ok {
		return "", false
	}
	return filepath.Join(sourceCacheDir(root, src), name), true
}

// remoteSourceOf reports the source an extension resolves from, or ok=false when
// it is local (or absent). Local always wins over a source.
func remoteSourceOf(root string, cfg extConfig, name string) (extSource, bool) {
	local := filepath.Join(root, cfg.extDir(), name)
	if _, err := os.Stat(filepath.Join(local, extensionManifest)); err == nil {
		return extSource{}, false
	}
	for _, s := range cfg.Sources {
		d := filepath.Join(sourceCacheDir(root, s), name)
		if _, err := os.Stat(filepath.Join(d, extensionManifest)); err == nil {
			return s, true
		}
	}
	return extSource{}, false
}

// ── enable / disable / list ──────────────────────────────────────────────────

func runExtensionEnable(g globalOpts, root, name string) error {
	cfg, err := loadExtConfig(root)
	if err != nil {
		return err
	}
	// An optional built-in ships with the binary, so it is trusted (no remote gate) and
	// scaffolds from the embed, not a local dir.
	if b, ok := optionalBuiltin(name); ok {
		if slices.Contains(cfg.Enabled, name) {
			fmt.Fprintf(os.Stderr, "extension %q already enabled\n", name)
		} else {
			cfg.Enabled = append(cfg.Enabled, name)
			if !g.dryRun {
				if err := saveExtConfig(root, cfg); err != nil {
					return err
				}
			}
			fmt.Fprintf(os.Stderr, "enabled %q (built-in)\n", name)
		}
		for _, f := range manifestConfigFindings(name, b.Manifest, os.Getenv) {
			fmt.Fprintf(os.Stderr, "  needs %s %q: %s\n", f.Kind, f.Name, f.Status)
		}
		warnMissingExtTools(b.Manifest)
		return applyExtensionFiles(g, b, root, false)
	}
	dir, ok := resolveExtensionDir(root, cfg, name)
	if !ok {
		return fmt.Errorf("no extension %q locally or in a synced source (run `llz extension sync` first?)", name)
	}
	m, err := readManifestAt(dir)
	if err != nil {
		return fmt.Errorf("read extension %q: %w", name, err)
	}
	if findings := append(lintManifest(m), lintKind(m, hasGoTests(dir))...); len(findings) > 0 {
		return fmt.Errorf("%q fails the ceiling — run `llz extension lint %s`", name, dir)
	}
	// Trust model: first-enable of a REMOTE extension runs third-party scaffolding,
	// so it is gated — refuse without --yes, naming the source + ref first.
	if src, remote := remoteSourceOf(root, cfg, name); remote && !slices.Contains(cfg.Enabled, name) && !g.yes {
		return fmt.Errorf("enabling %q scaffolds files from remote %s@%s — review it, then re-run with --yes", name, src.Repo, src.Ref)
	}
	if slices.Contains(cfg.Enabled, name) {
		fmt.Fprintf(os.Stderr, "extension %q already enabled\n", name)
	} else {
		cfg.Enabled = append(cfg.Enabled, name)
		if !g.dryRun {
			if err := saveExtConfig(root, cfg); err != nil {
				return err
			}
		}
		fmt.Fprintf(os.Stderr, "enabled %q\n", name)
	}
	// Configure-phase: surface the inputs the operator still owes (non-blocking —
	// secrets are usually set in CI, not here). `llz extension doctor` is the gate.
	for _, f := range manifestConfigFindings(name, m, os.Getenv) {
		fmt.Fprintf(os.Stderr, "  needs %s %q: %s\n", f.Kind, f.Name, f.Status)
	}
	warnMissingExtTools(m)                        // a declared tool that's absent → its check would silently skip
	return runExtensionApply(g, dir, root, false) // enable scaffolds, like the issue specifies
}

func runExtensionDisable(g globalOpts, root, name string) error {
	cfg, err := loadExtConfig(root)
	if err != nil {
		return err
	}
	if !slices.Contains(cfg.Enabled, name) {
		fmt.Fprintf(os.Stderr, "extension %q is not enabled\n", name)
		return nil
	}
	cfg.Enabled = slices.DeleteFunc(cfg.Enabled, func(s string) bool { return s == name })
	if !g.dryRun {
		if err := saveExtConfig(root, cfg); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "disabled %q (scaffolded files left in place — remove by hand if unwanted)\n", name)
	return nil
}

func runExtensionListEnabled(root string) error {
	cfg, err := loadExtConfig(root)
	if err != nil {
		return err
	}
	enabled := map[string]bool{}
	for _, n := range cfg.Enabled {
		enabled[n] = true
	}
	base := filepath.Join(root, cfg.extDir())
	entries, _ := os.ReadDir(base)

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tENABLED\tKIND\tSHORT")
	seen := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := readManifestAt(filepath.Join(base, e.Name()))
		if err != nil {
			continue // not an extension dir
		}
		seen[e.Name()] = true
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Name(), yesNo(enabled[e.Name()]), m.Kind, m.Short)
	}
	// Optional built-ins: shipped with the binary but off until enabled — list them so
	// `llz extension list` shows what is available to enable, not just what is local.
	for _, b := range builtinExtensionsFn() {
		if !b.Manifest.Optional || seen[b.Name] {
			continue
		}
		seen[b.Name] = true
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", b.Name, yesNo(enabled[b.Name]), b.Manifest.Kind, b.Manifest.Short+" (built-in)")
	}
	// enabled extensions resolved from a synced source (not a local subdir), plus
	// any that resolve nowhere (truly missing).
	for _, n := range cfg.Enabled {
		if seen[n] {
			continue
		}
		if dir, ok := resolveExtensionDir(root, cfg, n); ok {
			if m, err := readManifestAt(dir); err == nil {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n, "yes (source)", m.Kind, m.Short)
				continue
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n, "yes", "?", "MISSING — run `llz extension sync`")
	}
	return tw.Flush()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "—"
}

// ── apply / upgrade over the enabled set ─────────────────────────────────────

func runExtensionApplyAll(g globalOpts, root string, check bool) error {
	exts, err := loadAllExtensions(root) // built-ins + enabled — built-ins render from embed
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range exts {
		if len(e.Manifest.Files) == 0 {
			continue
		}
		if err := applyExtensionFiles(g, e, root, check); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func runExtensionUpgradeAll(g globalOpts, root string, check bool) error {
	exts, err := loadEnabledExtensions(root)
	if err != nil {
		return err
	}
	if len(exts) == 0 {
		fmt.Fprintln(os.Stderr, "no enabled extensions (see `llz extension enable`)")
		return nil
	}
	var firstErr error
	for _, e := range exts {
		if e.Source == "builtin" {
			continue // built-ins ship with the binary — no schema migration / copier wiring
		}
		if err := runExtensionUpgrade(g, e.Dir, root, check); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// enabledExtCommands gathers the Operate-phase commands: contributed by every
// enabled extension, ready to hand to addExtCommands (the ext.go registration
// path) at startup. A short is defaulted so the command is self-describing in
// help. Best-effort: callers warn-and-continue so a registry problem never breaks
// the whole CLI.
func enabledExtCommands(root string) ([]extCommand, error) {
	exts, err := loadEnabledExtensions(root)
	if err != nil {
		return nil, err
	}
	var cmds []extCommand
	for _, e := range exts {
		for _, c := range e.Manifest.Commands {
			if c.Short == "" {
				c.Short = fmt.Sprintf("%s (from extension %q)", c.Name, e.Name)
			}
			cmds = append(cmds, c)
		}
	}
	return cmds, nil
}

// enabledCIJobs flattens the ci: steps across every enabled extension — the
// system-mode input to the workflow generator.
func enabledCIJobs(root string) ([]extCIJob, error) {
	exts, err := loadEnabledExtensions(root)
	if err != nil {
		return nil, err
	}
	var jobs []extCIJob
	for _, e := range exts {
		jobs = append(jobs, manifestCIJobs(e.Manifest)...)
	}
	return jobs, nil
}
