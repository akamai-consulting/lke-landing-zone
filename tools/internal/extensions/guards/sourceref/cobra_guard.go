package sourceref

// cobra_guard.go — the CLI surface for guard.
//
// Split from guard.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func Cmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "source-ref-guard",
		Short: "fail when a documented tools/ path no longer exists",
		Long: "Scans the Markdown, YAML, shell, Go and Makefile text under --root for\n" +
			"`tools/...` path literals and fails when one names nothing on disk. Catches\n" +
			"the references that rot in PROSE — ADR bodies, YAML `#` comments, shell\n" +
			"headers, error-message string literals — which `llz ci docs-guard` cannot\n" +
			"see, because it checks Markdown links, flags, workflow dispatches and TOCs.\n\n" +
			"A literal containing `*` is treated as a pattern and must match at least one\n" +
			"entry, so a glob that has quietly stopped matching fails too.\n\n" +
			"Fix a finding by pointing it at the current location, or by rewriting the\n" +
			"sentence so it names no path. Historical notes should name the SYMBOL and\n" +
			"say where it moved, not preserve a dead path a reader will try to follow.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return Run(root) },
	}
	c.Flags().StringVar(&root, "root", ".", "repository root to scan")
	return c
}
