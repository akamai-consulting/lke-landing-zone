package main

// ci_docs_guard.go — `llz ci docs-guard`: fail CI when the docs describe a CLI, a
// workflow, or a repo layout that no longer exists.
//
// WHY IT EXISTS, what it checks and the traps it was built from all live in
// tools/internal/docsguard, along with every line of logic and every test. What is
// here is the flag set and the printing — and one argument that cannot move:
// `cmd.Root()`, the live cobra tree the documented `llz …` invocations are checked
// against. That argument is the reason this extension is internal Go rather than
// an argv tool someone could ship separately.

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/docsguard"
)

func ciDocsGuardCmd() *cobra.Command {
	var root string
	var opts docsguard.Options
	c := &cobra.Command{
		Use:   "docs-guard",
		Short: "fail when the docs name a command, flag, workflow input or path that does not exist",
		Long: "Validates every Markdown file against the repo it documents:\n" +
			"  • the FLAGS of every `llz …` invocation whose command resolves, against\n" +
			"    the live cobra tree (an unknown top-level command is NOT reported —\n" +
			"    `.llz/commands.yaml` lets an instance define its own)\n" +
			"  • every `gh workflow run` input, against the workflow's declared inputs\n" +
			"  • every relative link, in the template tree AND in the delivered\n" +
			"    (post-`deliver-docs`) operator set\n" +
			"  • every entry of a `<!-- toc -->` block, against that file's headings\n" +
			"Reports every finding, then exits 1. Catches the mechanical half of doc rot;\n" +
			"prose claims about behaviour still need a human.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep, err := docsguard.Run(root, opts, cmd.Root())
			if err != nil {
				return err
			}
			for _, f := range rep.Findings {
				fmt.Println(f)
			}
			if len(rep.Findings) > 0 {
				fmt.Printf("docs-guard: checked %s across %s.\n", rep.Scanned, rep.Scope())
				return fmt.Errorf("docs-guard: %d finding(s) across %s", len(rep.Findings), rep.Scope())
			}
			fmt.Printf("docs-guard: %d Markdown file(s) OK — checked %s.\n", rep.Read, rep.Scanned)
			return nil
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repo root to scan")
	c.Flags().BoolVar(&opts.SkipCommands, "skip-commands", false, "skip the llz command/flag check")
	c.Flags().BoolVar(&opts.SkipWorkflows, "skip-workflows", false, "skip the gh workflow run input check")
	c.Flags().BoolVar(&opts.SkipLinks, "skip-links", false, "skip the relative-link check")
	return c
}
