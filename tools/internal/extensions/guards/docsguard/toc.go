package docsguard

// toc.go — `llz ci gen-toc`: insert or refresh the delimited table of
// contents in a long Markdown document.
//
// WHY THIS IS GO AND NOT A SCRIPT. It shipped first as a 74-line Python script,
// and that was wrong twice over.
//
//  1. It re-implemented githubAnchor in a second language. The two rules then
//     disagreed with GitHub in the SAME way, so `docs-guard` compared a wrong
//     anchor against a wrong anchor and passed — the shared-assumption bug that
//     `ci_docs_guard.go`'s header warns about. Sharing docHeadings with the
//     checker means there is now exactly one slug implementation, and the
//     oracle test (TestGithubAnchor_MatchesGithubSlugger) is what keeps it
//     honest, since a checker cannot audit a rule it shares.
//
//  2. It spent the whole `python-scripts` untestable-LOC budget (74 of 60) on
//     logic that is plainly unit-testable. `.untestable-budget.yaml` says the
//     fix for that is to move the logic into the CLI, not to raise the budget.
//
// The TOC is delimited so it is REGENERABLE (this command rewrites what is
// between the markers) and CHECKABLE (checkDocTOCs verifies every entry still
// resolves). A table of contents is otherwise exactly the hand-maintained list
// this repo refuses.

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	tocOpen  = "<!-- toc -->"
	tocClose = "<!-- /toc -->"
)

// tocSkipHeading names headings that must never appear as TOC entries: the
// TOC's own heading (which would list itself) and the trailing pointer section
// every long doc here ends with, which is navigation rather than content.
var tocSkipHeading = map[string]bool{"contents": true, "see also": true}

var firstH2Re = regexp.MustCompile(`(?m)^##\s`)

// renderTOC builds the delimited block for a document, listing headings from
// level 2 down to maxLevel.
func renderTOC(body string, maxLevel int) string {
	var b strings.Builder
	b.WriteString(tocOpen + "\n## Contents\n\n")
	for _, h := range docHeadings(body) {
		if h.Level < 2 || h.Level > maxLevel {
			continue
		}
		if tocSkipHeading[strings.ToLower(strings.TrimSpace(stripInlineMarkup(h.Text)))] {
			continue
		}
		b.WriteString(fmt.Sprintf("%s- [%s](#%s)\n",
			strings.Repeat("  ", h.Level-2), stripInlineMarkup(h.Text), h.Anchor))
	}
	b.WriteString("\n" + tocClose)
	return b.String()
}

// stripInlineMarkup renders heading text as link text. Link syntax is unwrapped
// because a Markdown link cannot nest inside another link's text — the entry
// would render as literal brackets.
func stripInlineMarkup(s string) string {
	return strings.TrimSpace(anchorLink.ReplaceAllString(s, "$1"))
}

// applyTOC returns the document with its TOC block refreshed, or inserted just
// before the first `##` if it has none. Reports whether anything changed, so
// --check can fail without writing.
func applyTOC(body string, maxLevel int) (string, bool) {
	toc := renderTOC(body, maxLevel)
	if loc := tocBlockRe.FindStringIndex(body); loc != nil {
		out := body[:loc[0]] + toc + body[loc[1]:]
		return out, out != body
	}
	// Insert before the first level-2 heading, after the title and any intro
	// prose. A doc with no `##` at all gets no TOC — there is nothing to list.
	m := firstH2Re.FindStringIndex(body)
	if m == nil {
		return body, false
	}
	head := strings.TrimRight(body[:m[0]], "\n")
	return head + "\n\n" + toc + "\n\n" + body[m[0]:], true
}

// ApplyTOC returns the document with its TOC block refreshed, or inserted just
// before the first `##` if it has none, and reports whether anything changed.
//
// The caller decides what to do with a change — `llz ci gen-toc` writes it,
// `--check` fails on it. That split is why the file I/O is not here: it is the one
// part of this package that MUTATES, and keeping it in the command keeps the
// package itself file-in / findings-out, which is what lets `guard-docs` declare a
// gate binding honestly (see extension.go).
func ApplyTOC(body string, maxLevel int) (string, bool) { return applyTOC(body, maxLevel) }
