package budget

// ledger.go — the budget number and the prose above it must agree.
//
// THE GATE ALREADY DEMANDS THIS AND NOTHING VERIFIED IT. CoreSurfaceRemedy tells
// an author whose category grew to "update the number in {config} in THIS commit
// and say in the message why the code belongs in the layer that only wires
// commands up", and .core-surface-budget.yaml answers that with a LEDGER: a
// running list of `<number> / <n> files  <why>` lines above each budget, newest
// last. The file's own header calls the ledger the reason `exact: true` is worth
// its cost — "the paydown is banked rather than left as slack the next change
// grows into unremarked".
//
// Banked by hand, though. Raising `budget:` and not adding a line left the ledger
// silently one entry behind, and the gate went green because the only thing it
// compared was a count against a number. Caught the ordinary way: this session
// bumped 1,898 to 1,900 and added the line from memory, which is not a mechanism.
//
// WHAT IT CHECKS, and the narrowness is the point: a category that KEEPS a ledger
// must have the current budget as its LAST entry. One category keeps one today
// (cli-wiring-layer); cmd-llz-entrypoint states its six in words and
// .untestable-budget.yaml has none at all, and inventing the convention for them
// here would be a rule nobody asked for enforced against files that never opted
// in. A category with no ledger is not judged; a category with a ledger cannot
// drift from it.
//
// IT READS THE RAW BYTES rather than the parsed config, because comments are not
// in the parse. That is the same reason source-ref-guard exists one directory
// over: the claim and the value live on different sides of the parser, so nothing
// structured can compare them.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// categoryLine matches a category key at the config's two-space indent —
// `  cli-wiring-layer:` — and not the four-space keys inside one.
var categoryLine = regexp.MustCompile(`^  ([A-Za-z0-9_-]+):\s*$`)

// budgetLine matches the `budget: N` inside a category.
var budgetLine = regexp.MustCompile(`^\s+budget:\s*(\d+)\s*$`)

// ledgerLine matches one ledger entry: a comment whose first token is a count,
// optionally comma-grouped, followed by ` / <n> files`. The trailing prose is the
// reason and is deliberately unconstrained — this checks that the number was
// recorded, never what was said about it.
var ledgerLine = regexp.MustCompile(`^\s*#\s+([0-9][0-9,]*)\s*/\s*\d+\s+files\b`)

// LedgerFinding is one category whose ledger disagrees with its budget.
type LedgerFinding struct {
	Category string
	Budget   int
	Last     int // the last number the ledger records
	Line     int // 1-indexed line of the `budget:` key
}

// checkLedger returns a finding per category whose last ledger entry is not its
// current budget. Pure over the file's text so the interesting cases are
// unit-testable without a config on disk.
func checkLedger(config string) []LedgerFinding {
	var out []LedgerFinding

	var category string
	var lastLedger, budgetAt int
	haveLedger := false

	// flush judges the category that just ended. Called at each new category and
	// once at EOF, so the last category in the file is judged like every other —
	// an early version only flushed on transition and never checked the final one,
	// which in this config is the whole of cmd-llz-entrypoint.
	flush := func(budget int) {
		if category == "" || !haveLedger || budget == 0 {
			return
		}
		if lastLedger != budget {
			out = append(out, LedgerFinding{
				Category: category, Budget: budget, Last: lastLedger, Line: budgetAt,
			})
		}
	}

	var pendingBudget int
	for i, line := range strings.Split(config, "\n") {
		if m := categoryLine.FindStringSubmatch(line); m != nil {
			flush(pendingBudget)
			category, lastLedger, haveLedger, pendingBudget, budgetAt = m[1], 0, false, 0, 0
			continue
		}
		if m := ledgerLine.FindStringSubmatch(line); m != nil {
			n, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
			if err != nil {
				continue
			}
			lastLedger, haveLedger = n, true
			continue
		}
		if m := budgetLine.FindStringSubmatch(line); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			pendingBudget, budgetAt = n, i+1
		}
	}
	flush(pendingBudget)
	return out
}

// ledgerError renders the findings as the gate's error, or nil when they agree.
func ledgerError(configPath string, findings []LedgerFinding) error {
	if len(findings) == 0 {
		return nil
	}
	var b strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&b, "\n  %s: budget is %d but the ledger's last entry is %d",
			f.Category, f.Budget, f.Last)
	}
	return fmt.Errorf("%s: the budget and the ledger above it disagree.%s\n\n"+
		"The ledger is how a change to this number is banked — add a `<budget> / <n> files  <why>` "+
		"line recording what moved, in the SAME commit that changed the number. Editing the number "+
		"alone leaves the reason to be reconstructed later from a diff, which is the thing the "+
		"ledger exists to prevent", configPath, b.String())
}
