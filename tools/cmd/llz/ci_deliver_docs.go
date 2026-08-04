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
//
// VERSION PINNING, DELIBERATELY IN ONE PLACE: docs/README.md's pointer is pinned
// to the instance's template release, because that is the tree an operator browses
// to read the docs matching the code they run. The cross-doc links inside the kept
// files are NOT pinned — they track `main`. Pinning them meant every kept doc that
// merely mentioned secrets.md churned on every upgrade (27 of the 45 version-string
// lines in a real v0.0.31 → v0.0.32 diff), for a set of docs that is `managed` in
// .template-manifest and so is overwritten wholesale anyway — pure review noise
// with no merge value. The version-matched docs are the ones delivered LOCALLY
// (quickstart + runbooks + playbooks); what floats is the referenced set, where
// current is usually what you want and the pinned tree is one line away.

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
	var dir, org, ref, root, templateRoot string
	c := &cobra.Command{
		Use:   "deliver-docs",
		Short: "prune a copied-in docs/ to the operator set + write a version-pinned pointer to the rest",
		Long: "Slims an instance's docs/ to quickstart.md + runbooks/ + playbooks/ and writes\n" +
			"docs/README.md pointing at the full docs for the pinned template version in the\n" +
			"(public) template repo. Run after copying the template's docs/ in; the same verb\n" +
			"backs both the copier render step and release-e2e, so the keep-set can't drift.\n" +
			"With --template-root, ALSO repoints links in the instance's root-level Markdown\n" +
			"(README.md, AGENTS.md, …) that target template-only paths, which the docs/-scoped\n" +
			"rewrite never sees.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runDeliverDocs(dir, org, ref, root, templateRoot)
		},
	}
	c.Flags().StringVar(&dir, "docs", "docs", "the already-copied docs directory to prune in place")
	c.Flags().StringVar(&org, "org", "", "template org for the reference URL (e.g. akamai-consulting)")
	c.Flags().StringVar(&ref, "ref", "", "template ref/tag the instance is pinned to (for the version-matched URL)")
	c.Flags().StringVar(&root, "root", ".", "the instance root, whose top-level Markdown is repointed (needs --template-root)")
	c.Flags().StringVar(&templateRoot, "template-root", "", "path to the template checkout; enables the instance-root link repoint (copier passes _copier_conf.src_path)")
	return c
}

func runDeliverDocs(dir, org, ref, root, templateRoot string) error {
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
	// Rewrite those dead relative links to the template URL so they stay clickable;
	// links to docs that ARE still present stay relative.
	if err := repointReferencedLinks(dir, org); err != nil {
		return fmt.Errorf("repoint doc links: %w", err)
	}
	repointed := 0
	if templateRoot != "" {
		if repointed, err = repointInstanceRootLinks(root, dir, templateRoot, org); err != nil {
			return fmt.Errorf("repoint instance-root links: %w", err)
		}
	}
	sort.Strings(removed)
	fmt.Printf("deliver-docs: kept the operator set (quickstart + runbooks + playbooks); referenced %d other entr%s at the template repo.\n",
		len(removed), plural(len(removed), "y", "ies"))
	if repointed > 0 {
		fmt.Printf("deliver-docs: repointed %d instance-root link%s to template-only paths.\n",
			repointed, plural(repointed, "", "s"))
	}
	return nil
}

// ── the instance ROOT, which the docs/-scoped rewrite never sees ─────────────
//
// repointReferencedLinks walks only `docs/`, so a link from README.md or AGENTS.md
// into a path the instance does not carry stayed relative and stayed dead — in
// every rendered instance, forever. Three shipped that way: AGENTS.md →
// docs/adopter-guide.md (which this very command prunes), and
// apl-values/README.md → ../../platform-apl/ (a template-only tree).
//
// The rewrite is gated on the link existing IN THE TEMPLATE and NOT in the
// instance. That pairing is what makes it safe to run over arbitrary root-level
// Markdown: a link to something the instance creates LATER (landingzone.yaml,
// written by the first `llz env add`) is absent from both and so is left alone,
// rather than being repointed at a template URL that 404s.

// scaffoldDir is where the template keeps the files it renders INTO an instance.
// A file is template-owned iff the template ships it at instance-template/<rel>.
const scaffoldDir = "instance-template"

// repointInstanceRootLinks rewrites, in every TEMPLATE-OWNED .md under root except
// those under docsDir, the relative links that are dead in the instance but present
// in the template checkout. Returns how many links it rewrote.
//
// ONLY TEMPLATE-OWNED FILES ARE TOUCHED, and that is a correctness rule, not an
// optimisation. This runs on `copier update` against a LIVE instance, which holds
// plenty of Markdown that is none of our business: a `vendor/`ed chart, a
// `.terraform/` module cache, an adopter's own notes. An early cut of this walked
// all of them and rewrote a link inside `vendor/` — mutating a file the template
// does not own is exactly what .template-manifest exists to prevent. Gating on
// "the template ships this path under instance-template/" bounds the blast radius
// to the files copier itself rendered, with no denylist to keep current.
func repointInstanceRootLinks(root, docsDir, templateRoot, org string) (int, error) {
	if org == "" {
		org = "akamai-consulting"
	}
	// Identify docs/ by INODE, not by path string. --docs and --root are
	// independent flags: copier passes `--docs docs --root .` (both relative to
	// the instance), e2e passes `--docs .e2e-instance/docs --root .e2e-instance`.
	// Comparing cleaned path strings — or resolving one against the other — is
	// wrong for one caller or the other, and gets it wrong SILENTLY by walking
	// docs/ twice. os.SameFile sidesteps the spelling entirely.
	docsInfo, docsErr := os.Stat(docsDir)
	total := 0
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// docs/ is handled by repointReferencedLinks.
			if docsErr == nil {
				if fi, e := os.Stat(p); e == nil && os.SameFile(fi, docsInfo) {
					return filepath.SkipDir
				}
			}
			// Dot-directories (.git, .terraform, .instance-test) and vendored
			// trees hold no template-owned file, so skipping them is pure
			// walk cost avoided — the ownership check below is what makes it safe.
			if p != root {
				if strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				switch d.Name() {
				case "node_modules", "vendor":
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(p, ".md") {
			return nil
		}
		if relPath, e := filepath.Rel(root, p); e != nil ||
			!pathExists(filepath.Join(templateRoot, scaffoldDir, relPath)) {
			return nil // not ours to rewrite
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fileDir := filepath.Dir(rel)
		if fileDir == "." {
			fileDir = ""
		}
		out, n := rewriteInstanceRootLinks(string(data), fileDir,
			func(q string) bool { return pathExists(filepath.Join(root, q)) },
			func(q string) (bool, bool) {
				fi, e := os.Stat(filepath.Join(templateRoot, q))
				if e != nil {
					return false, false
				}
				return true, fi.IsDir()
			}, org)
		total += n
		if n > 0 {
			return os.WriteFile(p, []byte(out), 0o644)
		}
		return nil
	})
	return total, err
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// rewriteInstanceRootLinks repoints relative links that inInstance reports absent
// and inTemplate reports present, to the template repo at referencedDocsBranch.
// Directories get /tree/, files /blob/. Pure (both lookups are injected), so the
// "absent from BOTH is left alone" rule is unit-tested without a fixture tree.
func rewriteInstanceRootLinks(
	content, fileDir string,
	inInstance func(string) bool,
	inTemplate func(string) (exists bool, isDir bool),
	org string,
) (string, int) {
	n := 0
	out := mdLinkRe.ReplaceAllStringFunc(content, func(m string) string {
		target := m[2 : len(m)-1] // strip "](" … ")"
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
			strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") ||
			target == "" {
			return m
		}
		path, anchor := target, ""
		if i := strings.IndexByte(target, '#'); i >= 0 {
			path, anchor = target[:i], target[i:]
		}
		if path == "" {
			return m
		}
		resolved := filepath.Clean(filepath.Join(fileDir, path))
		if resolved == "." || strings.HasPrefix(resolved, "..") {
			return m // escapes the instance — not ours to rewrite
		}
		if inInstance(resolved) {
			return m // alive here; stays relative
		}
		exists, isDir := inTemplate(resolved)
		if !exists {
			// Dead in BOTH: a file the instance creates later (landingzone.yaml),
			// or a genuine typo. A template URL would 404 too, so leave it —
			// docs-guard reports it instead.
			return m
		}
		kind := "blob"
		if isDir {
			kind = "tree"
		}
		n++
		return fmt.Sprintf("](https://github.com/%s/lke-landing-zone/%s/%s/%s%s)",
			org, kind, referencedDocsBranch, resolved, anchor)
	})
	return out, n
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

The links to those docs from inside the local pages point at the template's
`+"`main`"+` — current, but possibly ahead of the release you run. This pointer is
the version-matched copy; read it here when the difference matters.
`, url)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// referencedDocsBranch is the branch the cross-doc links point at. Kept OUT of the
// version pin on purpose — see the pinning note at the top of this file.
const referencedDocsBranch = "main"

// repointReferencedLinks rewrites, in every kept .md, the relative links that
// point to a doc no longer present (a referenced one) so they target the template
// URL. Links to still-present docs stay relative.
func repointReferencedLinks(dir, org string) error {
	if org == "" {
		org = "akamai-consulting"
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
		if out := rewriteDocLinks(string(data), fileDir, present, org); out != string(data) {
			return os.WriteFile(p, []byte(out), 0o644)
		}
		return nil
	})
}

var mdLinkRe = regexp.MustCompile(`\]\(([^)]+)\)`)

// pinnedDocsPermalinkRe matches a cross-doc permalink already pinned to a concrete
// release — what an older llz wrote back when the ref was rendered into the links.
// Those are absolute, so the relative-link rewrite below skips them: without this,
// an instance delivered before the pin left the links keeps them forever, drifting
// further behind its own pointer with every upgrade (gsap-apl carried 27 stale
// v0.0.32 links into its v0.0.33 delivery).
var pinnedDocsPermalinkRe = regexp.MustCompile(`(github\.com/[^/\s)]+/lke-landing-zone/blob/)v\d+\.\d+\.\d+(/docs/)`)

// rewriteDocLinks repoints markdown links to referenced (now-absent) .md docs to
// the template URL at referencedDocsBranch, and normalises any already-absolute
// permalink an older delivery left pinned to a release. fileDir is the linking
// file's dir relative to docs/; present is the set of paths (relative to docs/)
// still delivered locally. Pure.
func rewriteDocLinks(content, fileDir string, present map[string]bool, org string) string {
	content = pinnedDocsPermalinkRe.ReplaceAllString(content, "${1}"+referencedDocsBranch+"${2}")
	base := fmt.Sprintf("https://github.com/%s/lke-landing-zone/blob/%s/docs", org, referencedDocsBranch)
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
