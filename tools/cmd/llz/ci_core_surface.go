package main

// ci_core_surface.go implements `llz ci core-surface`: the counterweight to
// `llz ci untestable-loc`, pointed the opposite way. That gate caps logic which
// cannot be unit-tested and names tools/cmd/llz as where it should go; this one
// caps how much lands there.
//
// WHY, AND WHAT THE NUMBER MEANS: docs/adr/0014-core-surface-budget.md.
// WHERE IT IS HEADED:              docs/designs/internal-extensions.md.
// The reasoning is not restated here.
//
// The engine, the Go counter and this gate's remedy string are gate-neutral or
// testable, so all three live in internal/budget. What is left here is the flag
// set — which, after the extraction this gate asked for and then received, is the
// whole of what a command should be.

import (
	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/budget"
)

func ciCoreSurfaceCmd() *cobra.Command {
	var configPath, root string
	var verbose bool
	c := &cobra.Command{
		Use:   "core-surface",
		Short: "fail when Go logic in package main exceeds the committed core-surface budget",
		Long: "Counts logic lines (non-blank, non-comment) of the non-test Go in\n" +
			"tools/cmd/llz and fails if it exceeds the budget in\n" +
			".core-surface-budget.yaml. The counterweight to `llz ci untestable-loc`:\n" +
			"that gate pushes logic INTO the CLI and names no ceiling, so package main\n" +
			"accretes. Satisfy this one by extracting to tools/internal/<pkg> (ADR 0013),\n" +
			"moving the capability out to an extension (issue #10), or deleting dead\n" +
			"code — and then ratchet the budget DOWN. Never raise it to go color.Green.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return budget.Run("core-surface", root, configPath, verbose, budget.CoreSurfaceRemedy)
		},
	}
	c.Flags().StringVar(&configPath, "config", ".core-surface-budget.yaml", "budget config file (relative to --root)")
	c.Flags().StringVar(&root, "root", ".", "repository root to scan")
	c.Flags().BoolVar(&verbose, "verbose", false, "list every file's count, not just over-budget categories")
	return c
}
