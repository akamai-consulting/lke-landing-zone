package main

// ci_gen_toc.go — `llz ci gen-toc`: insert or refresh the delimited table of
// contents in a long Markdown document.
//
// THE RENDERING IS IN internal/docsguard AND THE WRITE IS HERE, deliberately.
// Anchors come from the same docHeadings walk `llz ci docs-guard` checks against,
// which is the whole reason this is Go rather than the 74-line Python script it
// shipped as first: two implementations of the slug rule disagreed with GitHub in
// the SAME way, so the checker compared a wrong anchor against a wrong anchor and
// passed. One implementation is the fix, and it has to be the checker's.
//
// The file write stays out of that package so the package remains file-in /
// findings-out, which is what lets `guard-docs` declare `gate:scaffolded` honestly
// — a gate may hold `read-repo` and nothing else. See the extension declaration
// for what that leaves undeclared and why.

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/docsguard"
)

func ciGenTOCCmd() *cobra.Command {
	var maxLevel int
	var check bool
	c := &cobra.Command{
		Use:   "gen-toc <file.md>...",
		Short: "insert or refresh the <!-- toc --> block in a Markdown file",
		Long: "Rewrites the block between `<!-- toc -->` and `<!-- /toc -->`, or inserts\n" +
			"one before the first `##` heading. Anchors come from the same docHeadings\n" +
			"walk `llz ci docs-guard` checks against, so a generated TOC cannot disagree\n" +
			"with the checker.\n\n" +
			"Only add a TOC to a document long enough to need one — GitHub renders an\n" +
			"outline button, and a TOC nobody regenerates is worse than none.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var stale []string
			for _, path := range args {
				raw, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				out, changed := docsguard.ApplyTOC(string(raw), maxLevel)
				switch {
				case !changed:
					fmt.Printf("unchanged  %s\n", path)
				case check:
					stale = append(stale, path)
					fmt.Printf("STALE      %s\n", path)
				default:
					if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
						return err
					}
					fmt.Printf("written    %s\n", path)
				}
			}
			if len(stale) > 0 {
				return fmt.Errorf("gen-toc: %d file(s) have a stale table of contents — run `llz ci gen-toc` on them", len(stale))
			}
			return nil
		},
	}
	c.Flags().IntVar(&maxLevel, "level", 2, "deepest heading level to list (2 = ## only)")
	c.Flags().BoolVar(&check, "check", false, "report stale tables of contents instead of rewriting them")
	return c
}
