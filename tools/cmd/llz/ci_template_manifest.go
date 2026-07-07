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
)

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
			"(managed / merge / owned), or queries the class/list for callers that need\n" +
			"the same last-match-wins rules. Auto-detects instance-template/ in the\n" +
			"template repo, else .template-manifest in the current directory.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runTemplateManifest(root, classifyPath, listClass, os.Stdout, os.Stderr)
		},
	}
	c.Flags().StringVar(&root, "root", "", "scaffold root containing .template-manifest (default: auto-detect instance-template/ or .)")
	c.Flags().StringVar(&classifyPath, "classify", "", "print the update class for a scaffold-relative path")
	c.Flags().StringVar(&listClass, "list", "", "list scaffold files in the given class (managed|merge|owned)")
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
			return fmt.Errorf("template-manifest: unknown class %q (managed|merge|owned)", listClass)
		}
		for _, rel := range files {
			if m.classify(rel) == listClass {
				fmt.Fprintln(out, rel)
			}
		}
		return nil
	}

	counts := map[string]int{"managed": 0, "merge": 0, "owned": 0}
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
		fmt.Fprintf(errOut, "Add a rule for each (managed | merge | owned) — see the header in %s.\n", m.path)
		return fmt.Errorf("template-manifest: %d unclassified scaffold file(s)", len(unclassified))
	}
	fmt.Fprintf(out, "template-manifest: OK — managed=%d merge=%d owned=%d (%d files, all classified)\n",
		counts["managed"], counts["merge"], counts["owned"], len(files))
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
			return templateManifest{}, fmt.Errorf("template-manifest: %s:%d bad rule (expected `<managed|merge|owned>  <glob>`): %q", m.path, lineNo, line)
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
	return s == "managed" || s == "merge" || s == "owned"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
