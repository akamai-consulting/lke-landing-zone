package sourceref

// cobra_symbols.go — the CLI surface for symbols.
//
// Split from symbols.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

// SymbolsCmd builds the verb for the live CLI, where cmd.Root() is the real tree
// because main parented it.
func SymbolsCmd() *cobra.Command { return symbolsCmd(nil) }

// SymbolsCmdFor supplies the tree as DATA, for the driver, which runs each gate
// PARENTLESS — and a parentless command's Root() is itself. docs-guard's header
// records what that costs: it validated 868 invocations against a tree of one and
// reported clean. The ci-verb half here would do the same, indexing zero verbs and
// skipping every citation.
//
// Resolving cmd.Root() at CONSTRUCTION would be the same bug one step earlier: the
// ci group is not attached to the root until main finishes wiring, so the tree
// captured would be empty. It is read inside RunE instead.
func SymbolsCmdFor(tree *cobra.Command) *cobra.Command { return symbolsCmd(tree) }

func symbolsCmd(tree *cobra.Command) *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "symbol-ref-guard",
		Short: "fail when prose names a pkg.Symbol this repo does not export",
		Long: "Indexes every exported top-level identifier in this repo's Go tree, then\n" +
			"checks each `pkg.Symbol` reference under --root against it. The sibling of\n" +
			"`llz ci source-ref-guard`, and the half it cannot see: a reference can name\n" +
			"the right FILE and the wrong SYMBOL, which is what happens when code moves\n" +
			"packages and its callers are updated but its documentation is not.\n\n" +
			"In Go files only COMMENTS are scanned — code that names a moved symbol does\n" +
			"not compile, and a local variable sharing a package name (repo, registry,\n" +
			"render) would otherwise read as a package reference.\n\n" +
			"THE CONVENTION: `pkg.Symbol` is a LIVE pointer and must resolve; write\n" +
			"history possessively — `pkg's Symbol` — so \"it used to live in X\" stays\n" +
			"sayable without leaving a reference that only looks followable. Same for a\n" +
			"local variable that shares a package name: \"the platform argument's\n" +
			"Components\", not `platform.Components`.\n\n" +
			"A package this repo does not contain is skipped rather than reported: the\n" +
			"guard cannot see a third party's source, so a finding would only ever mean\n" +
			"'not vendored here'.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The injected tree wins; falling back to cmd.Root() keeps the live CLI
			// behaviour identical.
			t := tree
			if t == nil {
				t = cmd.Root()
			}
			return RunSymbols(root, t)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root to scan")
	return c
}
