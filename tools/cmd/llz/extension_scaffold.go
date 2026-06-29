package main

// extension_scaffold.go is the SCAFFOLD-phase router (issue #10) — the lifecycle
// counterpart to the ci: router (extension_ci.go). An extension's files: are
// rendered into the instance repo on `llz extension apply` and re-applied on
// upgrade. Outputs are recorded in .llz/extensions.lock so that (a) `--check`
// detects a hand-edited or missing scaffolded file (the "recipe output drift"
// from the issue) and (b) the owned paths can be fenced off from copier via
// _exclude (the 4th, "recipe-managed" ownership class). This is Phase 1
// (Scaffold), and proves the extension vehicle spans engines: embed/render here,
// Actions codegen in ci-workflow, copier migrations in upgrade.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

const extensionLockFile = ".llz/extensions.lock"

// extLock is the single lock (the issue's "extend the source lock to track
// outputs"): Sources pins each remote source's resolved SHA+digest, and Outputs
// records, per extension, the files it owns in the instance and their digests —
// sourceLock is the resolved pin recorded per source.
type sourceLock struct {
	Ref    string `json:"ref"`
	SHA    string `json:"sha"`
	Digest string `json:"digest"`
}

// the ownership + drift baseline.
type extLock struct {
	Sources map[string]sourceLock   `json:"sources,omitempty"`
	Outputs map[string][]lockedFile `json:"outputs,omitempty"`
}

type lockedFile struct {
	Path string `json:"path"`
	SHA  string `json:"sha256"`
	Mode string `json:"mode,omitempty"` // "" / "managed" (default) | "seed" (write-once, operator-owned)
}

func loadExtLock(root string) extLock {
	var l extLock
	if b, err := os.ReadFile(filepath.Join(root, extensionLockFile)); err == nil {
		_ = yaml.Unmarshal(b, &l)
	}
	if l.Outputs == nil {
		l.Outputs = map[string][]lockedFile{}
	}
	if l.Sources == nil {
		l.Sources = map[string]sourceLock{}
	}
	return l
}

func saveExtLock(root string, l extLock) error {
	b, err := yaml.Marshal(l)
	if err != nil {
		return err
	}
	p := filepath.Join(root, extensionLockFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// renderedFile is one file: entry rendered, ready to write or compare. Mode carries the
// extFile ownership class ("managed" | "seed"), normalized, so apply and --check can treat
// a seed file as write-once + operator-owned.
type renderedFile struct {
	Dst  string
	Body []byte
	SHA  string
	Mode string
}

// scaffoldVals builds the template context for an extension's files: the
// extension's own name plus the instance identity from .copier-answers.yml, so a
// file can reference <@ .name @> / <@ .instance_repo @> / <@ .upstream_org @>.
func scaffoldVals(root, name string) map[string]string {
	vals := map[string]string{"name": name}
	if a, _ := readAnswers(root); a != nil {
		vals["instance_repo"] = a.InstanceRepo
		vals["upstream_org"] = a.UpstreamOrg
	}
	return vals
}

// validateTargets renders the manifest's validate: step names as a YAML flow sequence
// (e.g. "[fmt-check, clippy, test]"), the value a scaffolded workflow interpolates into
// its matrix. Empty validate: yields "[]". Steps without a name are skipped (they have no
// matrix identity). This is what makes the validate: declaration load-bearing for an
// app-stage kit whose gates run in its own workflow rather than the platform gate.
func validateTargets(m extManifest) string {
	var names []string
	for _, s := range m.Validate {
		if s.Name != "" {
			names = append(names, s.Name)
		}
	}
	return "[" + strings.Join(names, ", ") + "]"
}

// extensionFromDir builds an Extension from an on-disk extension directory — the
// adapter the explicit-[dir] commands use to reach the value-based core.
func extensionFromDir(dir string) (Extension, error) {
	m, err := readManifestAt(dir)
	if err != nil {
		return Extension{}, err
	}
	return Extension{Name: m.Name, Source: "local", Dir: dir, fsys: os.DirFS(dir), Manifest: m}, nil
}

// renderScaffold renders every files: entry of an Extension through its fsys —
// origin-agnostic, so a built-in (embed.FS) and a local/remote (os.DirFS) render
// through identical code. Pure (no writes).
func renderScaffold(ext Extension, root string) (files []renderedFile, err error) {
	vals := scaffoldVals(root, ext.Name)
	for k, v := range varValues(ext.Manifest, os.Getenv) { // Configure → Scaffold: declared vars feed the render
		vals[k] = v
	}
	// Derived: the extension's own validate: step names as a YAML flow sequence, so an
	// app-stage workflow can render its CI matrix from the declared validate bar
	// (`target: <@ .validate_targets @>`) instead of hand-copying the list — making
	// validate: the single source for both the platform-visible gate and the app matrix.
	vals["validate_targets"] = validateTargets(ext.Manifest)
	for _, f := range ext.Manifest.Files {
		rendered, rerr := renderEntry(ext.fsys, f, vals)
		if rerr != nil {
			return nil, rerr
		}
		files = append(files, rendered...)
	}
	return files, nil
}

// renderEntry renders one files: entry. When Src is a regular file it yields one output;
// when Src is a DIRECTORY it walks the subtree and yields one output per file, each
// rendered to Dst joined with the file's path relative to Src. The directory form lets a
// workload kit scaffold a whole tree (e.g. a Cargo workspace) without hand-listing every
// file — and because it flattens to the same per-file renderedFile list, drift/--check,
// the lock, and teardown all keep working unchanged.
func renderEntry(fsys fs.FS, f extFile, vals map[string]string) ([]renderedFile, error) {
	info, err := fs.Stat(fsys, f.Src)
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", f.Src, err)
	}
	mode := fileMode(f.Mode)
	if !info.IsDir() {
		out, rerr := renderBytes(fsys, f.Src, vals)
		if rerr != nil {
			return nil, fmt.Errorf("render %s: %w", f.Src, rerr)
		}
		return []renderedFile{{Dst: f.Dst, Body: out, SHA: digest(out), Mode: mode}}, nil
	}
	var out []renderedFile
	walkErr := fs.WalkDir(fsys, f.Src, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return werr
		}
		body, rerr := renderBytes(fsys, p, vals)
		if rerr != nil {
			return fmt.Errorf("render %s: %w", p, rerr)
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, f.Src), "/") // fs paths are slash-separated
		out = append(out, renderedFile{Dst: path.Join(f.Dst, rel), Body: body, SHA: digest(body), Mode: mode})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil // WalkDir yields lexical order → deterministic lock
}

// runExtensionApply is the explicit-[dir] adapter over applyExtensionFiles.
func runExtensionApply(g globalOpts, extDir, root string, check bool) error {
	ext, err := extensionFromDir(extDir)
	if err != nil {
		return err
	}
	return applyExtensionFiles(g, ext, root, check)
}

// applyExtensionFiles renders an Extension's files: into root. --check writes
// nothing and exits non-zero on drift (a scaffolded file hand-edited, missing, or
// orphaned); --dry-run previews. On a real apply it records the outputs in the
// lock so drift is measurable and copier can be fenced off the owned paths.
func applyExtensionFiles(g globalOpts, ext Extension, root string, check bool) error {
	name := ext.Name
	files, err := renderScaffold(ext, root)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "extension has no files: — nothing to scaffold")
		return nil
	}

	if check {
		var drift []string
		manifestSet := map[string]bool{}
		for _, f := range files {
			manifestSet[f.Dst] = true
			if f.Mode == "seed" {
				continue // write-once + operator-owned: a missing/edited seed file is not drift
			}
			got, rerr := os.ReadFile(filepath.Join(root, f.Dst))
			switch {
			case rerr != nil:
				drift = append(drift, f.Dst+" (missing — never scaffolded)")
			case digest(got) != f.SHA:
				drift = append(drift, f.Dst+" (modified since scaffold)")
			}
		}
		for _, lf := range loadExtLock(root).Outputs[name] { // orphans the manifest dropped
			if !manifestSet[lf.Path] {
				drift = append(drift, lf.Path+" (orphaned — extension no longer ships it)")
			}
		}
		if len(drift) == 0 {
			fmt.Fprintf(os.Stderr, "extension %q: scaffold in sync (%d file(s))\n", name, len(files))
			return nil
		}
		fmt.Fprintf(os.Stderr, "extension %q: %d scaffold drift(s):\n", name, len(drift))
		for _, x := range drift {
			fmt.Fprintf(os.Stderr, "  • %s\n", x)
		}
		return fmt.Errorf("scaffold drift in %q — run `llz extension apply`", name)
	}

	if g.dryRun {
		for _, f := range files {
			fmt.Fprintf(os.Stderr, "→ (dry-run) would write %s\n", filepath.Join(root, f.Dst))
		}
		return nil
	}

	var locked []lockedFile
	for _, f := range files {
		dst := filepath.Join(root, f.Dst)
		// A seed file is write-once: if it already exists, the operator owns it — record
		// it (so exclude/teardown still know we own the path) but never overwrite.
		if f.Mode == "seed" {
			if _, statErr := os.Stat(dst); statErr == nil {
				locked = append(locked, lockedFile{Path: f.Dst, SHA: f.SHA, Mode: "seed"})
				fmt.Fprintf(os.Stderr, "seed %s already present — left as-is\n", dst)
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, f.Body, 0o644); err != nil {
			return err
		}
		locked = append(locked, lockedFile{Path: f.Dst, SHA: f.SHA, Mode: f.Mode})
		fmt.Fprintf(os.Stderr, "scaffolded %s\n", dst)
	}
	lock := loadExtLock(root)
	lock.Outputs[name] = locked
	if err := saveExtLock(root, lock); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "extension %q: %d file(s) recorded in %s (run `llz extension exclude` to fence copier off them)\n", name, len(locked), extensionLockFile)
	return nil
}

// ownedPaths returns the de-duplicated, sorted set of instance paths every
// extension owns, per the lock. Pure, so the exclude emitter is testable.
func ownedPaths(l extLock) []string {
	seen := map[string]bool{}
	var paths []string
	for _, files := range l.Outputs {
		for _, f := range files {
			if !seen[f.Path] {
				seen[f.Path] = true
				paths = append(paths, f.Path)
			}
		}
	}
	sort.Strings(paths)
	return paths
}

// runExtensionExclude prints the extension-owned paths as a copier _exclude block.
// Paths the template doesn't ship are already left alone by `copier update`; this
// matters for any path an extension owns that the template ALSO manages, so the
// two delivery channels don't fight over it.
func runExtensionExclude(root string) error {
	paths := ownedPaths(loadExtLock(root))
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "no extension-owned paths recorded (run `llz extension apply` first)")
		return nil
	}
	fmt.Println("# Extension-owned paths (from " + extensionLockFile + "). Add to the template's")
	fmt.Println("# copier.yml _exclude so `copier update` never clobbers a path an extension owns.")
	fmt.Println("_exclude:")
	for _, p := range paths {
		fmt.Printf("  - %q\n", p)
	}
	return nil
}
