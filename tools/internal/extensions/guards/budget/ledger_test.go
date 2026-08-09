package budget

import (
	"strings"
	"testing"
)

// The shape .core-surface-budget.yaml actually uses: a running ledger above the
// number, newest last, comma-grouped.
const ledgerConfig = `categories:
  cli-wiring-layer:
    kind: go-logic
    #   1,889 / 112 files  the state before the move.
    #   1,898 / 113 files  THE MOVE OUT OF package main.
    #   1,900 / 113 files  +2 for a new gate's wiring.
    budget: 1900
    include:
      - "tools/internal/cli/*.go"
`

func TestLedgerAgreesWhenTheLastEntryIsTheBudget(t *testing.T) {
	if got := checkLedger(ledgerConfig); len(got) != 0 {
		t.Errorf("the last entry is the budget; expected no findings, got %+v", got)
	}
}

// The defect this exists for: the number moved and the ledger did not, which the
// gate could not see because the only thing it compared was a count to a number.
func TestLedgerCatchesABudgetRaisedWithoutAnEntry(t *testing.T) {
	got := checkLedger(strings.Replace(ledgerConfig, "budget: 1900", "budget: 1950", 1))
	if len(got) != 1 {
		t.Fatalf("expected one finding, got %+v", got)
	}
	if got[0].Budget != 1950 || got[0].Last != 1900 {
		t.Errorf("finding should carry both numbers, got %+v", got[0])
	}
	if got[0].Category != "cli-wiring-layer" {
		t.Errorf("finding should name the category, got %q", got[0].Category)
	}
}

// A paydown is banked the same way a raise is: the ledger records the new,
// LOWER number. Dropping the budget without an entry is the same drift.
func TestLedgerCatchesAPaydownWithoutAnEntry(t *testing.T) {
	if got := checkLedger(strings.Replace(ledgerConfig, "budget: 1900", "budget: 1850", 1)); len(got) != 1 {
		t.Errorf("a lowered budget with no entry must be caught too, got %+v", got)
	}
}

// Not every category opts into the convention: cmd-llz-entrypoint states its six
// in words and .untestable-budget.yaml has no ledger at all. Judging them would
// enforce a rule against files that never adopted it.
func TestCategoryWithoutALedgerIsNotJudged(t *testing.T) {
	cfg := `categories:
  cmd-llz-entrypoint:
    kind: go-logic
    # The entry point holds the main symbol and the os.Exit. Six lines.
    budget: 6
    include:
      - "tools/cmd/llz/*.go"
`
	if got := checkLedger(cfg); len(got) != 0 {
		t.Errorf("a category with no ledger must not be judged, got %+v", got)
	}
}

// Each category is judged against its OWN ledger. An early version carried the
// previous category's last entry across the boundary, which made a correct
// second category report against the first one's number.
func TestLedgersDoNotLeakBetweenCategories(t *testing.T) {
	cfg := ledgerConfig + `  cmd-llz-entrypoint:
    kind: go-logic
    # Six lines, stated in words.
    budget: 6
    include:
      - "tools/cmd/llz/*.go"
`
	if got := checkLedger(cfg); len(got) != 0 {
		t.Errorf("the second category has no ledger of its own; got %+v", got)
	}
}

// The LAST category in the file is judged like every other. An early version
// flushed only on a category transition, so whatever sat at EOF went unchecked —
// and in the real config that is a whole category.
func TestTheFinalCategoryInTheFileIsJudged(t *testing.T) {
	cfg := `categories:
  a:
    # 10 / 1 files  first.
    budget: 10
  b:
    # 20 / 2 files  second.
    budget: 99
`
	got := checkLedger(cfg)
	if len(got) != 1 || got[0].Category != "b" {
		t.Fatalf("the category at EOF must be judged, got %+v", got)
	}
}

func TestLedgerErrorNamesTheConfigAndBothNumbers(t *testing.T) {
	err := ledgerError(".core-surface-budget.yaml", []LedgerFinding{
		{Category: "cli-wiring-layer", Budget: 1950, Last: 1900},
	})
	if err == nil {
		t.Fatal("findings must produce an error")
	}
	for _, want := range []string{".core-surface-budget.yaml", "cli-wiring-layer", "1950", "1900"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got %v", want, err)
		}
	}
}

func TestLedgerErrorIsNilWhenTheyAgree(t *testing.T) {
	if err := ledgerError(".core-surface-budget.yaml", nil); err != nil {
		t.Errorf("no findings must produce no error, got %v", err)
	}
}
