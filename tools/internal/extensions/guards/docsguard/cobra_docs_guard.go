package docsguard

// cobra_docs_guard.go — `llz ci docs-guard`: fail CI when the docs describe a CLI, a
// workflow, or a repo layout that no longer exists.
//
// WHY IT EXISTS, what it checks and the traps it was built from all live in
// tools/internal/extensions/guards/docsguard, along with every line of logic and every test. What is
// here is the flag set and the printing — and one argument that cannot move:
// `cmd.Root()`, the live cobra tree the documented `llz …` invocations are checked
// against. That argument is the reason this extension is internal Go rather than
// an argv tool someone could ship separately.

import (
	"fmt"

	"github.com/spf13/cobra"
)

// DocsGuardCmd builds the verb for the live CLI, where cmd.Root() is the real
// tree because main parented it.
func DocsGuardCmd() *cobra.Command { return docsGuardCmd(nil) }

// DocsGuardCmdFor builds the verb with the tree supplied as DATA, for a caller
// that runs it PARENTLESS.
//
// IT EXISTS BECAUSE A DRIVER SILENTLY DISABLED THIS GUARD'S LARGEST CHECK.
// `llz ci gates` builds each gate with its constructor and Execute()s it without a
// parent — and a parentless cobra command's Root() is ITSELF. So this guard
// validated 868 documented `llz …` invocations against a tree containing one
// command, resolved none of them, skipped all of them, and reported clean.
//
// THE OBVIOUS FIX DOES NOT WORK, and is recorded so nobody spends the hour again:
// attaching the command to the real root before Execute() causes INFINITE
// RECURSION, because cobra's Execute() on a PARENTED command delegates to
// Root().ExecuteC(), which re-runs `llz ci gates` from os.Args. The tree has to
// arrive as a value, not as a parent.
func DocsGuardCmdFor(tree *cobra.Command) *cobra.Command { return docsGuardCmd(tree) }

func docsGuardCmd(tree *cobra.Command) *cobra.Command {
	var root string
	var opts Options
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
			// The injected tree wins; falling back to cmd.Root() keeps the live CLI
			// behaviour identical.
			t := tree
			if t == nil {
				t = cmd.Root()
			}
			rep, err := Run(root, opts, t)
			if err != nil {
				return err
			}
			// FAIL CLOSED ON AN EMPTY CORPUS, the rule plaintext-guard and
			// wave-dependency-guard already state in their own words: "a guard that
			// had nothing to check reports the same green as one that checked
			// everything, so this fails instead."
			//
			// THIS GUARD DID NOT HAVE IT, AND THAT IS WHY IT WAS INVISIBLE. Run
			// through a driver that handed it a one-command tree, it resolved zero of
			// 868 documented invocations, found nothing, and returned nil. The
			// asymmetry with its siblings is the whole reason nobody noticed for a
			// week — so the check moves here rather than staying a convention some
			// guards keep.
			//
			// Guarded on OTHER work having happened: links and TOC entries come from
			// the same Markdown, so a corpus that resolved hundreds of those and ZERO
			// commands did not have a command-free docs tree — it had a broken tree.
			// A bare Invocations==0 check would fail on a legitimately command-free
			// repo, and a guard that fails on a true state is one people delete.
			// NOTHING AT ALL is the first case, and my first version of this check
			// missed it: it fired on "commands zero, links non-zero", which an EMPTY
			// TREE fails to satisfy because both are zero. A guard pointed at the
			// wrong directory would have sailed through the very check written to
			// stop it. Caught by the driver-wide empty-corpus test, which is the
			// argument for that test existing rather than trusting each guard.
			if rep.Scanned == (Scanned{}) {
				return fmt.Errorf("docs-guard examined NOTHING under %q — no markdown, no "+
					"links, no commands. A guard that had nothing to check reports the same "+
					"green as one that checked everything, so this fails instead", root)
			}
			if rep.Scanned.Invocations == 0 && rep.Scanned.Links+rep.Scanned.TOCEntries > 0 {
				return fmt.Errorf("docs-guard resolved %d link(s) and %d toc entr(ies) but ZERO "+
					"`llz …` invocations — the command tree it was given is empty or wrong, so "+
					"the largest check in this guard silently did nothing. Refusing to report "+
					"clean; see DocsGuardCmdFor", rep.Scanned.Links, rep.Scanned.TOCEntries)
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
