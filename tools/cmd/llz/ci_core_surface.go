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
// The engine — config parse, glob walk, per-category tally, verdict, report — is
// gate-neutral and lives in ci_budget_gate.go. This file adds only what is
// specific to this gate: the command, its defaults, its remedy, and the Go
// counter.

import (
	"github.com/spf13/cobra"
)

// coreSurfaceRemedy is the guidance printed on a breach, and the ONLY copy of it:
// .core-surface-budget.yaml deliberately sets no `remedy:` key, because when the
// wording lived in both places the YAML copy silently won and edits here never
// reached an operator.
//
// It has two branches because the number is a high-water mark with no slack, so a
// breach is routine rather than exceptional. Preferred: decompose, and the number
// goes DOWN. Otherwise: record the growth on the same line in the same commit,
// where a reviewer sees it next to the code that caused it. Telling authors never
// to raise it — untestable-loc's doctrine — would be wrong here, and would get the
// gate deleted rather than obeyed.
const coreSurfaceRemedy = "Package main grew (ADR 0014). Prefer to shrink it: extract to " +
	"tools/internal/<pkg> (ADR 0013), move the capability out to an extension (issue #10), " +
	"or delete what is dead. If the growth is intended, update the number in {config} in THIS " +
	"commit and say in the message why the code belongs in package main."

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
			"code — and then ratchet the budget DOWN. Never raise it to go green.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCoreSurface(root, configPath, verbose)
		},
	}
	c.Flags().StringVar(&configPath, "config", ".core-surface-budget.yaml", "budget config file (relative to --root)")
	c.Flags().StringVar(&root, "root", ".", "repository root to scan")
	c.Flags().BoolVar(&verbose, "verbose", false, "list every file's count, not just over-budget categories")
	return c
}

func runCoreSurface(root, configPath string, verbose bool) error {
	return runBudgetGate("core-surface", root, configPath, verbose, coreSurfaceRemedy)
}

// countGoLogicLines counts non-blank, non-comment lines of Go — the counter
// behind the core-surface budget (ADR 0014).
//
// It deliberately does NOT track /* … */ block comments. Go in this repo is
// documented with `//` (godoc's form), so a block-comment state machine would
// buy nothing — and it would MISFIRE, because `/*` appears here almost only
// inside string literals: glob patterns (`"tools/cmd/llz/*.go"`,
// `"terraform-iac-bootstrap/*/.terraform.lock.hcl"`) and regexes. A naive
// scanner treats the first of those as an unterminated comment and swallows the
// rest of the file, which undercounts by more than half. Matching the sibling
// counters' rule — blank, or the line STARTS with the comment marker — has
// neither failure mode, and leaves `x := 1 // note` counted as the logic it is.
func countGoLogicLines(content string) int {
	return countLinesSkippingComments(content, "//")
}
