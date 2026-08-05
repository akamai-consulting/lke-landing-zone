package main

// ci_untestable_loc.go implements `llz ci untestable-loc` — the design-principle
// gate this repo ratchets against: logic belongs in unit-testable Go, not in
// inline workflow bash, standalone shell scripts, or untested Python. The command
// counts "logic lines" (non-blank, non-comment) per category and fails when any
// category exceeds the budget declared in a committed config file (default
// .untestable-budget.yaml at the repo root). Budgets ratchet DOWN over time as
// more bash moves into the CLI; a PR that adds untestable code instead of
// converting it pushes a category over budget and is rejected in CI.
//
// EVERYTHING THIS GATE KNOWS NOW LIVES IN internal/budget — the config parse, the
// glob walk, the per-category tally, the verdict, the report, the counters and
// the remedy. What is left here is a cobra flag set, which is all a command
// should be. That move is `guard-budgets`, the first entry in the catalog's
// suggested order (docs/designs/internal-extensions.md), and it was prescribed by
// this gate's own counterweight: `llz ci core-surface` said "extract to
// tools/internal/<pkg>", so it was.

import (
	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/budget"
)

func ciUntestableLOCCmd() *cobra.Command {
	var configPath, root string
	var verbose bool
	c := &cobra.Command{
		Use:   "untestable-loc",
		Short: "fail when inline-bash / shell / python / tf-provisioner logic exceeds the committed budget",
		Long: "Counts logic lines (non-blank, non-comment) of inline workflow bash,\n" +
			"shell scripts, Python scripts, and Terraform provisioner heredocs, and\n" +
			"fails if any category exceeds the budget in .untestable-budget.yaml. The\n" +
			"design principle: logic belongs in unit-testable Go (tools/), not in CI\n" +
			"shell. Budgets ratchet DOWN as code is converted — a PR that grows a\n" +
			"category over budget is rejected so reviewers can ask for the logic to\n" +
			"move into the llz CLI (or, for genuine install/glue, an explicit entry\n" +
			"under `exclude:`).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return budget.Run("untestable-loc", root, configPath, verbose, budget.UntestableRemedy)
		},
	}
	c.Flags().StringVar(&configPath, "config", ".untestable-budget.yaml", "budget config file (relative to --root)")
	c.Flags().StringVar(&root, "root", ".", "repository root to scan")
	c.Flags().BoolVar(&verbose, "verbose", false, "list every file's count, not just over-budget categories")
	return c
}
