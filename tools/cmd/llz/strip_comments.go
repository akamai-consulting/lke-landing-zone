package main

// strip_comments.go — `llz ci strip-comments`: remove YAML-level comment lines
// from the workflow files delivered into an instance, so the copied-in surface
// carries only the machinery, not the ~38% of rationale comments that every
// instance inherits but no operator reads (the template SOURCE keeps them). Run
// as a copier _tasks step after render; the design/rationale lives in the
// template repo + docs/designs, not in each instance's generated .github/.
//
// SAFETY: only true YAML comments (a line whose first non-space char is `#`,
// OUTSIDE a block scalar) are removed. Lines inside a `key: |` / `key: >` block
// scalar — bash in `run:`, markdown echoed to $GITHUB_STEP_SUMMARY, heredoc
// bodies — are LITERAL content and are never touched, even if they start with
// `#`. The leading file-header comment block is kept (a pointer to what the file
// is + where the rationale lives). Idempotent.

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

func ciStripCommentsCmd() *cobra.Command {
	var keepHeader bool
	c := &cobra.Command{
		Use:   "strip-comments <file>...",
		Short: "remove YAML-level comment lines from delivered workflow files (keeps run:/block-scalar content)",
		Long: "Strips true YAML comments (first non-space char `#`, outside a block scalar)\n" +
			"from each file in place, leaving the leading header block. Content inside\n" +
			"`|`/`>` block scalars (bash, markdown, heredocs) is never touched. Used by the\n" +
			"copier _tasks render step to slim the copied-in workflow surface; the template\n" +
			"source keeps its comments. Idempotent.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, files []string) error {
			for _, f := range files {
				data, err := os.ReadFile(f)
				if err != nil {
					return fmt.Errorf("read %s: %w", f, err)
				}
				out := stripYAMLComments(string(data), keepHeader)
				if err := os.WriteFile(f, []byte(out), 0o644); err != nil {
					return fmt.Errorf("write %s: %w", f, err)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&keepHeader, "keep-header", true, "keep the leading contiguous comment block (the file header)")
	return c
}

// blockScalarOpen matches a mapping/sequence entry whose value is a block scalar
// (`|` or `>`, with optional chomp/indent indicator and an optional trailing
// comment): the body that follows is literal text, indented deeper than the key.
var blockScalarOpen = regexp.MustCompile(`^(\s*)(?:-\s+)?(?:[^\s#][^:]*:\s*|-\s*)[|>][+-]?\d*\s*(?:#.*)?$`)

// stripYAMLComments removes YAML comment-only lines that sit OUTSIDE any block
// scalar. Block-scalar bodies (literal text) are preserved verbatim. When
// keepHeader is set, the leading contiguous comment block is preserved.
func stripYAMLComments(content string, keepHeader bool) string {
	lines := strings.Split(content, "\n")
	var out []string

	inScalar := false
	scalarIndent := 0 // indent of the block-scalar KEY; body must be deeper
	inHeader := keepHeader

	for _, ln := range lines {
		trimmed := strings.TrimLeft(ln, " \t")
		indent := len(ln) - len(trimmed)
		blank := strings.TrimSpace(ln) == ""

		if inScalar {
			// The block scalar continues through blank lines and any line indented
			// deeper than its key. A non-blank line at/under the key indent ends it.
			if blank || indent > scalarIndent {
				out = append(out, ln) // literal body — never stripped
				continue
			}
			inScalar = false // fell out of the scalar; fall through to normal handling
		}

		// Leading header block: keep comments until the first non-comment,
		// non-blank line, then header mode is over.
		if inHeader {
			if blank || strings.HasPrefix(trimmed, "#") {
				out = append(out, ln)
				continue
			}
			inHeader = false
		}

		// A YAML comment-only line outside a scalar → drop it.
		if !blank && strings.HasPrefix(trimmed, "#") {
			continue
		}

		out = append(out, ln)

		// Does THIS line open a block scalar? Then its body is literal.
		if m := blockScalarOpen.FindStringSubmatch(ln); m != nil {
			inScalar = true
			scalarIndent = len(m[1])
		}
	}

	// Collapse the runs of blank lines a comment-strip leaves behind, so the result
	// stays tidy (never more than one consecutive blank).
	return collapseBlankLines(strings.Join(out, "\n"))
}

func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	prevBlank := false
	for _, ln := range lines {
		blank := strings.TrimSpace(ln) == ""
		if blank && prevBlank {
			continue
		}
		out = append(out, ln)
		prevBlank = blank
	}
	return strings.Join(out, "\n")
}
