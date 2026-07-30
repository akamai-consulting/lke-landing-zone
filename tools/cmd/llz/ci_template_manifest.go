package main

// ci_template_manifest.go implements `llz ci template-manifest` — the native
// port of template-scripts/check-template-manifest.sh. It keeps
// instance-template/.template-manifest honest by verifying that every scaffold
// file has an update class (managed / merge / owned), and it provides the same
// path classifier used by humans and update tooling.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// upgradeAction is what `llz upgrade` does to a file of a given class once
// `copier update` has run. It is the manifest's operational meaning: the class
// name is the label, this is the behaviour.
type upgradeAction string

const (
	// upgradeOverwrite re-copies the file from a clean render of the target
	// template version, discarding whatever the instance had.
	upgradeOverwrite upgradeAction = "overwrite"
	// upgradeMerge leaves copier's own 3-way merge result in place.
	upgradeMerge upgradeAction = "merge"
	// upgradeRestore puts the instance's pre-update bytes back.
	upgradeRestore upgradeAction = "restore"
)

// templateClass is the single definition of what an update class MEANS: what
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
type templateClass struct {
	name    string
	upgrade upgradeAction
	// copierFenced: the class carries instance-authored content, so `copier
	// update` must not re-render + 3-way-merge it. Enforced against copier.yml's
	// _skip_if_exists/_exclude by checkCopierFencing.
	copierFenced bool
	// digestLocked: the template owns the bytes outright and an upgrade
	// overwrites them, so a local edit is silently lost — record a digest and
	// fail CI on drift instead. Only meaningful for token-free files (a copier
	// token makes the rendered bytes per-instance); see writeManagedLock.
	digestLocked bool
	summary      string
}

var templateClasses = []templateClass{
	{name: "managed", upgrade: upgradeOverwrite, digestLocked: true,
		summary: "template owns it outright — an upgrade overwrites it"},
	{name: "merge", upgrade: upgradeMerge,
		summary: "template owns the logic, the file carries fork-local tokens — 3-way-merged"},
	{name: "owned", upgrade: upgradeRestore, copierFenced: true,
		summary: "the instance owns it — an upgrade never touches it"},
}

func lookupTemplateClass(name string) (templateClass, bool) {
	for _, c := range templateClasses {
		if c.name == name {
			return c, true
		}
	}
	return templateClass{}, false
}

// templateClassNames renders the class set for help and error text, so adding a
// row to templateClasses updates every message that lists the classes.
func templateClassNames() string {
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

type templateManifest struct {
	root  string
	path  string
	rules []templateManifestRule
}

func ciTemplateManifestCmd() *cobra.Command {
	var root, classifyPath, listClass string
	c := &cobra.Command{
		Use:   "template-manifest",
		Short: "validate or query the scaffold .template-manifest update classes",
		Long: "Validates that every scaffold file is classified by .template-manifest\n" +
			"(" + templateClassNames() + "), or queries the class/list for callers that need\n" +
			"the same last-match-wins rules. Auto-detects instance-template/ in the\n" +
			"template repo, else .template-manifest in the current directory.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runTemplateManifest(root, classifyPath, listClass, os.Stdout, os.Stderr)
		},
	}
	c.Flags().StringVar(&root, "root", "", "scaffold root containing .template-manifest (default: auto-detect instance-template/ or .)")
	c.Flags().StringVar(&classifyPath, "classify", "", "print the update class for a scaffold-relative path")
	c.Flags().StringVar(&listClass, "list", "", "list scaffold files in the given class ("+templateClassNames()+")")
	return c
}

func runTemplateManifest(root, classifyPath, listClass string, out, errOut io.Writer) error {
	if classifyPath != "" && listClass != "" {
		return fmt.Errorf("template-manifest: use only one of --classify or --list")
	}
	m, err := loadTemplateManifest(root)
	if err != nil {
		return err
	}

	if classifyPath != "" {
		cls := m.classify(classifyPath)
		if cls == "" {
			fmt.Fprintf(errOut, "%s: UNCLASSIFIED\n", classifyPath)
			return fmt.Errorf("template-manifest: %s is unclassified", classifyPath)
		}
		fmt.Fprintln(out, cls)
		return nil
	}

	files, err := scaffoldManifestFiles(m.root)
	if err != nil {
		return err
	}

	if listClass != "" {
		if !validTemplateClass(listClass) {
			return fmt.Errorf("template-manifest: unknown class %q (%s)", listClass, templateClassNames())
		}
		for _, rel := range files {
			if m.classify(rel) == listClass {
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
		cls := m.classify(rel)
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
		fmt.Fprintf(errOut, "Add a rule for each (%s) — see the header in %s.\n", templateClassNames(), m.path)
		return fmt.Errorf("template-manifest: %d unclassified scaffold file(s)", len(unclassified))
	}
	if err := m.checkCopierFencing(files, errOut); err != nil {
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
func (m templateManifest) checkCopierFencing(files []string, errOut io.Writer) error {
	copierPath := filepath.Join(filepath.Dir(m.root), "copier.yml")
	if !fileExists(copierPath) {
		if alt := filepath.Join(filepath.Dir(m.root), "copier.yaml"); fileExists(alt) {
			copierPath = alt
		} else {
			return nil // instance context (or no copier config) — nothing to check
		}
	}
	data, err := os.ReadFile(copierPath)
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
		c, ok := lookupTemplateClass(m.classify(rel))
		if !ok || !c.copierFenced {
			continue
		}
		if p.AnswersFile != "" && rel == p.AnswersFile {
			continue // copier regenerates the answers tracker itself
		}
		covered := false
		for _, g := range protect {
			if matchGlob(g, rel) {
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
	return nil
}

func loadTemplateManifest(root string) (templateManifest, error) {
	if root == "" {
		switch {
		case fileExists(filepath.FromSlash("instance-template/.template-manifest")):
			root = "instance-template"
		case fileExists(".template-manifest"):
			root = "."
		default:
			return templateManifest{}, fmt.Errorf("template-manifest: .template-manifest not found (looked in instance-template/ and .)")
		}
	}
	root = filepath.Clean(root)
	manifestPath := filepath.Join(root, ".template-manifest")
	if root == "." {
		manifestPath = ".template-manifest"
	}

	f, err := os.Open(manifestPath)
	if err != nil {
		return templateManifest{}, fmt.Errorf("template-manifest: read %s: %w", manifestPath, err)
	}
	defer f.Close()

	m := templateManifest{root: root, path: filepath.ToSlash(manifestPath)}
	s := bufio.NewScanner(f)
	lineNo := 0
	for s.Scan() {
		lineNo++
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 || !validTemplateClass(parts[0]) {
			return templateManifest{}, fmt.Errorf("template-manifest: %s:%d bad rule (expected `<%s>  <glob>`): %q", m.path, lineNo, templateClassNames(), line)
		}
		m.rules = append(m.rules, templateManifestRule{class: parts[0], pattern: parts[1]})
	}
	if err := s.Err(); err != nil {
		return templateManifest{}, fmt.Errorf("template-manifest: read %s: %w", m.path, err)
	}
	if len(m.rules) == 0 {
		return templateManifest{}, fmt.Errorf("template-manifest: %s defines no rules", m.path)
	}
	return m, nil
}

func (m templateManifest) classify(rel string) string {
	rel = filepath.ToSlash(filepath.Clean(rel))
	rel = strings.TrimPrefix(rel, "./")
	var hit string
	for _, rule := range m.rules {
		if matchGlob(rule.pattern, rel) {
			hit = rule.class
		}
	}
	return hit
}

func scaffoldManifestFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
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

func validTemplateClass(s string) bool {
	_, ok := lookupTemplateClass(s)
	return ok
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
