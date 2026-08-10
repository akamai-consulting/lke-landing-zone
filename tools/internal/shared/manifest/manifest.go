// Package manifest is .template-manifest: which files the template owns, what
// class each is in, and what `copier update` is allowed to do to it.
//
// ADR 0014 PINS THIS AS THE SINGLE OWNERSHIP AUTHORITY, which is exactly why it
// should not have been inside an extension. `selfupgrade` reads the class table to
// decide what an upgrade may overwrite or restore, `lint` and `upgrade` load it to
// check drift -- three peers depending on a capability for the one fact the whole
// upgrade path is defined against.
//
// WHAT STAYED BEHIND is Run, the `llz ci template-manifest` verb: it takes an
// io.Writer pair and prints a classification report for a human. Reading the
// manifest is substrate; reporting on it is a command.
package manifest

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/pathglob"
)

type UpgradeAction string

const (
	// UpgradeOverwrite re-copies the file from a clean render of the target
	// template version, discarding whatever the instance had.
	UpgradeOverwrite UpgradeAction = "overwrite"
	// upgradeMerge leaves copier's own 3-way merge result in place.
	upgradeMerge UpgradeAction = "merge"
	// UpgradeRestore puts the instance's pre-update bytes back.
	UpgradeRestore UpgradeAction = "restore"
)

// Class is the single definition of what an update class MEANS: what
// `llz upgrade` does to a file in it, whether `copier update` must be fenced off
// it, and whether its bytes are recorded in the digest lock.
//
// Those three facts used to live in three files as scattered `classify(rel) ==
// "owned"` string comparisons — this classifier, upgrade_policy.go, and the
// digest lock — so .template-manifest, copier.yml and the lock could disagree
// with nothing noticing. Everything now reads this table, which makes the
// manifest the one ownership authority and the lock a projection of it rather
// than a second, separately-scoped system. A NEW class is one row here, not a
// fourth mechanism.
type Class struct {
	name    string
	Upgrade UpgradeAction
	// copierFenced: the class carries instance-authored content, so `copier
	// update` must not re-render + 3-way-merge it. Enforced against copier.yml's
	// _skip_if_exists/_exclude by checkCopierFencing.
	copierFenced bool
	// DigestLocked: the template owns the bytes outright and an upgrade
	// overwrites them, so a local edit is silently lost — record a digest and
	// fail CI on drift instead. Only meaningful for token-free files (a copier
	// token makes the rendered bytes per-instance); see writeManagedLock.
	DigestLocked bool
	summary      string
}

var templateClasses = []Class{
	{name: "managed", Upgrade: UpgradeOverwrite, DigestLocked: true,
		summary: "template owns it outright — an upgrade overwrites it"},
	{name: "merge", Upgrade: upgradeMerge,
		summary: "template owns the logic, the file carries fork-local tokens — 3-way-merged"},
	{name: "owned", Upgrade: UpgradeRestore, copierFenced: true,
		summary: "the instance owns it — an upgrade never touches it"},
}

func LookupClass(name string) (Class, bool) {
	for _, c := range templateClasses {
		if c.name == name {
			return c, true
		}
	}
	return Class{}, false
}

// ClassNames renders the class set for help and error text, so adding a
// row to templateClasses updates every message that lists the classes.
func ClassNames() string {
	names := make([]string, len(templateClasses))
	for i, c := range templateClasses {
		names[i] = c.name
	}
	return strings.Join(names, "|")
}

type templateManifestRule struct {
	class   string
	pattern string
}

type Manifest struct {
	Root  string
	path  string
	rules []templateManifestRule
}

// copierProtect holds the copier.yml keys that decide whether `copier update`
// leaves an already-existing file alone. `_skip_if_exists` files are re-copied
// only when absent (so an existing one is never re-rendered / merged);
// `_exclude` files are not part of the template at all; `_answers_file` is the
// tracker copier regenerates itself (never a content merge).
type copierProtect struct {
	SkipIfExists []string `yaml:"_skip_if_exists"`
	Exclude      []string `yaml:"_exclude"`
	AnswersFile  string   `yaml:"_answers_file"`
}

// checkCopierFencing enforces the invariant behind every `copierFenced` class
// (today: `owned`): such a file carries instance-authored content, so `copier
// update` must NOT re-render + 3-way-merge it — every fenced scaffold file that the
// template actually ships must be covered by copier's `_skip_if_exists` (or
// `_exclude`). When it isn't, copier merges the template's version onto the
// instance's divergent content and can leave conflict markers behind — the class
// that shipped invalid YAML in the gsap-apl v0.0.24 upgrade
// (docs/designs/cross-org-reuse-pattern.md Phase 0). The manifest declares intent;
// this makes copier.yml honor it, closing the gap where the two silently disagree.
//
// Only runs when copier.yml is found next to the scaffold root (the template
// repo); in an instance (no copier.yml) it is a no-op, since there is nothing to
// keep consistent there.
// checkCopierFencing reads copier.yml from the scaffold root's PARENT, which is
// why a caller fencing this must root at the repository and not at the scaffold:
// a reader fenced at instance-template/ would refuse ../copier.yml and the check
// would silently become the "no copier config" no-op below.
func (m Manifest) checkCopierFencing(r fileReader, files []string, errOut io.Writer) error {
	copierPath := filepath.Join(filepath.Dir(m.Root), "copier.yml")
	if !existsVia(r, copierPath) {
		if alt := filepath.Join(filepath.Dir(m.Root), "copier.yaml"); existsVia(r, alt) {
			copierPath = alt
		} else {
			return nil // instance context (or no copier config) — nothing to check
		}
	}
	data, err := r.ReadFile(copierPath)
	if err != nil {
		return fmt.Errorf("template-manifest: read %s: %w", copierPath, err)
	}
	var p copierProtect
	if err := yaml.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("template-manifest: parse %s: %w", copierPath, err)
	}
	protect := append(append([]string{}, p.SkipIfExists...), p.Exclude...)

	var viol []string
	for _, rel := range files {
		c, ok := LookupClass(m.Classify(rel))
		if !ok || !c.copierFenced {
			continue
		}
		if p.AnswersFile != "" && rel == p.AnswersFile {
			continue // copier regenerates the answers tracker itself
		}
		covered := false
		for _, g := range protect {
			if pathglob.Match(g, rel) {
				covered = true
				break
			}
		}
		if !covered {
			viol = append(viol, rel)
		}
	}
	if len(viol) > 0 {
		fmt.Fprintf(errOut, "::error::%d copier-fenced scaffold file(s) are not protected by copier's _skip_if_exists/_exclude in %s:\n", len(viol), copierPath)
		for _, rel := range viol {
			fmt.Fprintf(errOut, "  - %s\n", rel)
		}
		fmt.Fprintf(errOut, "`copier update` would re-render + 3-way-merge these onto the instance's own\n"+
			"content (risking committed conflict markers). Add each to copier.yml `_skip_if_exists`,\n"+
			"or reclassify it in %s if the template should in fact own it.\n", m.path)
		return fmt.Errorf("template-manifest: %d copier-fenced file(s) not protected by copier", len(viol))
	}
	return m.checkCopierFencingReverse(files, p, copierPath, errOut)
}

// checkCopierFencingReverse is the other half of the containment: every scaffold
// file copier SKIPS must be a class that wants skipping.
//
// The forward pass asks "is every fenced file protected?". Without this one,
// nothing asks the converse, and the gap is not cosmetic: a `managed` or `merge`
// file listed in `_skip_if_exists` is pinned at whatever version the instance
// first rendered. Copier skips it, so no update reaches it; and the manifest
// classes it template-owned, so `llz upgrade`'s restore pass does not put a newer
// version back either. The file silently stops receiving template changes, and
// both files individually look correct — the disagreement only exists BETWEEN
// them. That is precisely the class of drift the one-class-table refactor set out
// to make impossible, so the check belongs with it.
//
// SCOPE: `_skip_if_exists` only, not `_exclude`. They mean different things.
// `_skip_if_exists` is the fence — "copier will not update this" — which is an
// ownership claim, and ownership is the manifest's business. `_exclude` removes a
// file from the template altogether, which is a DELIVERY decision: a file that
// never ships has no meaningful update class, so demanding one would reject a
// legitimate "keep this out of instances" rule.
//
// ADR 0014 records why this matters beyond hygiene: the extension framework
// (issue #10) needs to fence copier off extension-owned files, and PR #15
// currently does it with a second, runtime `copier update --exclude` built from
// the extension lock. Two fences can disagree. Making the containment
// bidirectional means a fenced path that the manifest does not know about cannot
// pass silently.
func (m Manifest) checkCopierFencingReverse(files []string, p copierProtect, copierPath string, errOut io.Writer) error {
	var mislabelled []string
	for _, rel := range files {
		if p.AnswersFile != "" && rel == p.AnswersFile {
			continue // copier regenerates the answers tracker itself
		}
		if !pathglob.MatchAny(p.SkipIfExists, rel) {
			continue
		}
		if c, ok := LookupClass(m.Classify(rel)); ok && c.copierFenced {
			continue
		}
		mislabelled = append(mislabelled, rel)
	}
	if len(mislabelled) > 0 {
		fmt.Fprintf(errOut, "::error::%d scaffold file(s) are fenced by copier's _skip_if_exists in %s "+
			"but are not a copier-fenced class in %s:\n", len(mislabelled), copierPath, m.path)
		for _, rel := range mislabelled {
			fmt.Fprintf(errOut, "  - %s (class %q)\n", rel, m.Classify(rel))
		}
		fmt.Fprintf(errOut, "Copier will never update these, but %s says the template owns them — so they\n"+
			"are pinned at whatever the instance first rendered and no upgrade reaches them.\n"+
			"Reclassify each as a copier-fenced class (%s), or drop it from `_skip_if_exists`.\n",
			m.path, fencedClassNames())
		return fmt.Errorf("template-manifest: %d copier-fenced path(s) the manifest does not own", len(mislabelled))
	}
	// A pattern matching nothing is dead config, and this repo normally FAILS on
	// unused registry entries. Not here: a fence rule may legitimately anticipate a
	// file the template does not ship yet (copier applies it the moment one
	// appears), so an unmatched rule is reported and not enforced.
	for _, g := range p.SkipIfExists {
		matched := false
		for _, rel := range files {
			if pathglob.Match(g, rel) {
				matched = true
				break
			}
		}
		if !matched {
			fmt.Fprintf(errOut, "note: copier `_skip_if_exists` rule %q in %s matches no scaffold file.\n", g, copierPath)
		}
	}
	return nil
}

// fencedClassNames lists the classes that copier must be fenced off, for error
// text. Derived from the class table so a new fenced class needs no edit here.
func fencedClassNames() string {
	var names []string
	for _, c := range templateClasses {
		if c.copierFenced {
			names = append(names, c.name)
		}
	}
	return strings.Join(names, "|")
}

// fileReader is every call this package makes against the disk, named so one body
// serves a fenced caller and an unfenced one. capability.Repo satisfies it
// structurally — no adapter and no second copy of the parser, which is how the
// guard and the verbs stay guaranteed to read a manifest the same way.
type fileReader interface {
	ReadFile(string) ([]byte, error)
	Stat(string) (os.FileInfo, error)
	WalkDir(string, fs.WalkDirFunc) error
}

// osReader is the unfenced reader, for callers outside the capability model:
// internal/verbs (which declares no bindings by design) and cmd/llz.
type osReader struct{}

func (osReader) ReadFile(p string) ([]byte, error)         { return os.ReadFile(p) }
func (osReader) Stat(p string) (os.FileInfo, error)        { return os.Stat(p) }
func (osReader) WalkDir(p string, fn fs.WalkDirFunc) error { return filepath.WalkDir(p, fn) }

// Load reads .template-manifest, unfenced. See osReader for who that is for.
func Load(root string) (Manifest, error) { return LoadFrom(osReader{}, root) }

// LoadFrom is Load through a caller's own reader.
func LoadFrom(r fileReader, root string) (Manifest, error) {
	if root == "" {
		switch {
		case existsVia(r, filepath.FromSlash("instance-template/.template-manifest")):
			root = "instance-template"
		case existsVia(r, ".template-manifest"):
			root = "."
		default:
			return Manifest{}, fmt.Errorf("template-manifest: .template-manifest not found (looked in instance-template/ and .)")
		}
	}
	root = filepath.Clean(root)
	manifestPath := filepath.Join(root, ".template-manifest")
	if root == "." {
		manifestPath = ".template-manifest"
	}

	raw, err := r.ReadFile(manifestPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("template-manifest: read %s: %w", manifestPath, err)
	}

	m := Manifest{Root: root, path: filepath.ToSlash(manifestPath)}
	s := bufio.NewScanner(bytes.NewReader(raw))
	lineNo := 0
	for s.Scan() {
		lineNo++
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 || !ValidClass(parts[0]) {
			return Manifest{}, fmt.Errorf("template-manifest: %s:%d bad rule (expected `<%s>  <glob>`): %q", m.path, lineNo, ClassNames(), line)
		}
		m.rules = append(m.rules, templateManifestRule{class: parts[0], pattern: parts[1]})
	}
	if err := s.Err(); err != nil {
		return Manifest{}, fmt.Errorf("template-manifest: read %s: %w", m.path, err)
	}
	if len(m.rules) == 0 {
		return Manifest{}, fmt.Errorf("template-manifest: %s defines no rules", m.path)
	}
	return m, nil
}

func (m Manifest) Classify(rel string) string {
	rel = filepath.ToSlash(filepath.Clean(rel))
	rel = strings.TrimPrefix(rel, "./")
	var hit string
	for _, rule := range m.rules {
		if pathglob.Match(rule.pattern, rel) {
			hit = rule.class
		}
	}
	return hit
}

// ScaffoldFiles lists the scaffold, unfenced. See osReader.
func ScaffoldFiles(root string) ([]string, error) { return ScaffoldFilesFrom(osReader{}, root) }

// ScaffoldFilesFrom is ScaffoldFiles through a caller's own reader.
func ScaffoldFilesFrom(r fileReader, root string) ([]string, error) {
	var files []string
	err := r.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".terraform":
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("template-manifest: walk %s: %w", root, err)
	}
	sort.Strings(files)
	return files, nil
}

// existsVia is fileExists through a reader.
func existsVia(r fileReader, p string) bool {
	_, err := r.Stat(p)
	return err == nil
}

func ValidClass(s string) bool {
	_, ok := LookupClass(s)
	return ok
}

// Run is `llz ci template-manifest`'s body, and it lives HERE rather than with the
// cobra command because it reaches into this package's internals -- the manifest's
// source path, the class table, the copier-fencing check. Splitting it out meant
// exporting four things for exactly one caller, which is API widening dressed as
// layering. The extension keeps the command and the declaration; the report is
// part of what a manifest can tell you about itself.
func Run(root, classifyPath, listClass string, out, errOut io.Writer) error {
	return RunFrom(osReader{}, root, classifyPath, listClass, out, errOut)
}

// RunFrom is Run through a caller's own reader — the door template-manifest's
// gate goes through.
func RunFrom(r fileReader, root, classifyPath, listClass string, out, errOut io.Writer) error {
	if classifyPath != "" && listClass != "" {
		return fmt.Errorf("template-manifest: use only one of --classify or --list")
	}
	m, err := LoadFrom(r, root)
	if err != nil {
		return err
	}

	if classifyPath != "" {
		cls := m.Classify(classifyPath)
		if cls == "" {
			fmt.Fprintf(errOut, "%s: UNCLASSIFIED\n", classifyPath)
			return fmt.Errorf("template-manifest: %s is unclassified", classifyPath)
		}
		fmt.Fprintln(out, cls)
		return nil
	}

	files, err := ScaffoldFilesFrom(r, m.Root)
	if err != nil {
		return err
	}

	if listClass != "" {
		if !ValidClass(listClass) {
			return fmt.Errorf("template-manifest: unknown class %q (%s)", listClass, ClassNames())
		}
		for _, rel := range files {
			if m.Classify(rel) == listClass {
				fmt.Fprintln(out, rel)
			}
		}
		return nil
	}

	counts := map[string]int{}
	for _, c := range templateClasses {
		counts[c.name] = 0
	}
	var unclassified []string
	for _, rel := range files {
		cls := m.Classify(rel)
		if cls == "" {
			unclassified = append(unclassified, rel)
			continue
		}
		counts[cls]++
	}
	if len(unclassified) > 0 {
		fmt.Fprintf(errOut, "::error::%d scaffold file(s) match no rule in %s:\n", len(unclassified), m.path)
		for _, rel := range unclassified {
			fmt.Fprintf(errOut, "  - %s\n", rel)
		}
		fmt.Fprintf(errOut, "Add a rule for each (%s) — see the header in %s.\n", ClassNames(), m.path)
		return fmt.Errorf("template-manifest: %d unclassified scaffold file(s)", len(unclassified))
	}
	if err := m.checkCopierFencing(r, files, errOut); err != nil {
		return err
	}
	var tally []string
	for _, c := range templateClasses {
		tally = append(tally, fmt.Sprintf("%s=%d", c.name, counts[c.name]))
	}
	fmt.Fprintf(out, "template-manifest: OK — %s (%d files, all classified)\n",
		strings.Join(tally, " "), len(files))
	return nil
}
