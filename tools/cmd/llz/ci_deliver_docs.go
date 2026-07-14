package main

// ci_deliver_docs.go — `llz ci deliver-docs`: shrink an instance's copied-in
// docs/ to the day-to-day operator set (quickstart + runbooks + playbooks) and
// REFERENCE the rest at the (public) template repo, version-pinned to this
// instance. The full architecture/spec/design docs don't need to live in every
// instance — they're one `git tree` link away at the pinned template version.
//
// This is the single source of truth for "what docs an instance carries",
// invoked by BOTH delivery paths — the copier _tasks render step AND release-e2e's
// docs hoist — so the keep-set can't drift between them (the drift that leaked
// cross-org-reuse-pattern.md into the e2e instance and broke instantiate).
//
// It operates on an ALREADY-COPIED docs dir: the caller `cp -a`s the template's
// full docs/, then this prunes to the keep-set and writes docs/README.md pointing
// at the rest. Idempotent.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// docsKeep is the day-to-day operator set an instance carries locally. Everything
// else in docs/ is referenced at the template repo. Defined ONCE here so both
// delivery paths agree.
var docsKeep = map[string]bool{
	"quickstart.md": true, // stand up + operate this instance
	"runbooks":      true, // incident recovery
	"playbooks":     true, // routine operational how-tos
	"README.md":     true, // the pointer this verb writes (kept if it already exists)
}

func ciDeliverDocsCmd() *cobra.Command {
	var dir, org, ref string
	c := &cobra.Command{
		Use:   "deliver-docs",
		Short: "prune a copied-in docs/ to the operator set + write a version-pinned pointer to the rest",
		Long: "Slims an instance's docs/ to quickstart.md + runbooks/ + playbooks/ and writes\n" +
			"docs/README.md pointing at the full docs for the pinned template version in the\n" +
			"(public) template repo. Run after copying the template's docs/ in; the same verb\n" +
			"backs both the copier render step and release-e2e, so the keep-set can't drift.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runDeliverDocs(dir, org, ref)
		},
	}
	c.Flags().StringVar(&dir, "docs", "docs", "the already-copied docs directory to prune in place")
	c.Flags().StringVar(&org, "org", "", "template org for the reference URL (e.g. akamai-consulting)")
	c.Flags().StringVar(&ref, "ref", "", "template ref/tag the instance is pinned to (for the version-matched URL)")
	return c
}

func runDeliverDocs(dir, org, ref string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read docs dir %s: %w", dir, err)
	}
	var removed []string
	for _, e := range entries {
		if docsKeep[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("prune %s: %w", e.Name(), err)
		}
		removed = append(removed, e.Name())
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(docsPointer(org, ref)), 0o644); err != nil {
		return fmt.Errorf("write docs/README.md: %w", err)
	}
	// Kept docs still link to now-referenced ones (e.g. quickstart → secrets.md).
	// Rewrite those dead relative links to the versioned template URL so they stay
	// clickable; links to docs that ARE still present stay relative.
	if err := repointReferencedLinks(dir, org, ref); err != nil {
		return fmt.Errorf("repoint doc links: %w", err)
	}
	sort.Strings(removed)
	fmt.Printf("deliver-docs: kept the operator set (quickstart + runbooks + playbooks); referenced %d other entr%s at the template repo.\n",
		len(removed), plural(len(removed), "y", "ies"))
	return nil
}

// docsPointer renders the docs/README.md that replaces the referenced docs. org
// and ref default sensibly so a hand-run without flags still produces a usable
// (if unpinned) pointer.
func docsPointer(org, ref string) string {
	if org == "" {
		org = "akamai-consulting"
	}
	if ref == "" {
		ref = "main"
	}
	url := fmt.Sprintf("https://github.com/%s/lke-landing-zone/tree/%s/docs", org, ref)
	return fmt.Sprintf(`# Documentation

This instance carries the day-to-day operator docs locally:

- **quickstart.md** — stand up and operate this instance
- **runbooks/** — incident recovery
- **playbooks/** — routine operational how-tos

The full documentation set for **your pinned template version** — architecture,
secrets, the LandingZone spec, adopter guide, environments/promotion, design docs,
alerting, and more — lives in the (public) template repo, versioned to match this
instance so it never drifts from the code you run:

> %s

It is *referenced* rather than copied in to keep this instance small. `+"`llz upgrade`"+`
re-pins this pointer to your new template version.
`, url)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// repointReferencedLinks rewrites, in every kept .md, the relative links that
// point to a doc no longer present (a referenced one) so they target the
// versioned template URL. Links to still-present docs stay relative.
func repointReferencedLinks(dir, org, ref string) error {
	if org == "" {
		org = "akamai-consulting"
	}
	if ref == "" {
		ref = "main"
	}
	present := map[string]bool{}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if rel, e := filepath.Rel(dir, p); e == nil {
				present[rel] = true
			}
		}
		return nil
	})
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fileDir := filepath.Dir(rel)
		if fileDir == "." {
			fileDir = ""
		}
		if out := rewriteDocLinks(string(data), fileDir, present, org, ref); out != string(data) {
			return os.WriteFile(p, []byte(out), 0o644)
		}
		return nil
	})
}

var mdLinkRe = regexp.MustCompile(`\]\(([^)]+)\)`)

// rewriteDocLinks repoints markdown links to referenced (now-absent) .md docs to
// the template URL. fileDir is the linking file's dir relative to docs/; present
// is the set of paths (relative to docs/) still delivered locally. Pure.
func rewriteDocLinks(content, fileDir string, present map[string]bool, org, ref string) string {
	base := fmt.Sprintf("https://github.com/%s/lke-landing-zone/blob/%s/docs", org, ref)
	return mdLinkRe.ReplaceAllStringFunc(content, func(m string) string {
		target := m[2 : len(m)-1] // strip "](" … ")"
		if !strings.Contains(target, ".md") ||
			strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
			strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
			return m
		}
		path, anchor := target, ""
		if i := strings.IndexByte(target, '#'); i >= 0 {
			path, anchor = target[:i], target[i:]
		}
		if !strings.HasSuffix(path, ".md") {
			return m
		}
		resolved := filepath.Clean(filepath.Join(fileDir, path))
		if strings.HasPrefix(resolved, "..") { // escapes docs/ — leave as-is
			return m
		}
		if present[resolved] { // still delivered locally — keep relative
			return m
		}
		return fmt.Sprintf("](%s/%s%s)", base, resolved, anchor)
	})
}
