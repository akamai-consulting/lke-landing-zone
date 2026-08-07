package main

// Mutation-resistant tests for the budget engine: the branches where a plausible
// edit changes the VERDICT rather than the wording, and which the behavioural
// tests in ci_core_surface_test.go do not already pin.

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeBudgetRepo is THE fixture for every budget-gate test: a temp repo holding
// the given files plus a config written verbatim to b.yaml.
//
// It is one function because three near-copies had already accumulated across two
// test files, two of them carrying a byte-identical write loop. That is the shape
// guard_walk.go was extracted to end — five guards each with their own copy of one
// loop, and the copies had diverged. Divergence in a fixture is worse than in
// production code: the tests still pass, they just quietly stop testing the same
// thing.
func writeBudgetRepo(t *testing.T, cfg string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "b.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// allowEmpty opts out of the matched-nothing check ONLY. A mutant that lets it
// short-circuit the whole category — the easiest way to write this wrong — would
// silently stop enforcing the budget on every category that declares it, which is
// the failure the matched-nothing check exists to prevent, reintroduced by its own
// escape hatch.
func TestAllowEmptyDoesNotSuppressAnOverBudgetFailure(t *testing.T) {
	root := writeBudgetRepo(t,
		"categories:\n  core:\n    kind: go-logic\n    budget: 1\n    allowEmpty: true\n"+
			"    include:\n      - \"tools/cmd/llz/*.go\"\n",
		map[string]string{"tools/cmd/llz/a.go": "package main\nvar x = 1\nvar y = 2\n"})

	var out, errOut bytes.Buffer
	if err := runBudgetGateTo("core-surface", root, "b.yaml", false, coreSurfaceRemedy, &out, &errOut); err == nil {
		t.Fatal("allowEmpty must not excuse a category that is over budget")
	}
}

// `matched` counts files the globs SELECTED, not files that scored above zero.
// Deriving it from the scored set instead (len(r.files)) is the natural-looking
// mistake, and it would call a live category dead the moment its files happened to
// tally zero — retiring, for example, terraform-provisioner-bash, which sits at a
// legitimate 0 in the real config today.
func TestMatchedCountsSelectedFilesNotScoringFiles(t *testing.T) {
	root := writeBudgetRepo(t,
		"categories:\n  core:\n    kind: go-logic\n    budget: 0\n    include:\n      - \"tools/cmd/llz/*.go\"\n",
		map[string]string{"tools/cmd/llz/a.go": "// comments only\n// so the tally is zero\n"})

	cfg, err := loadBudgetConfig(filepath.Join(root, "b.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	results, err := scanBudgetCategories(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 category, got %d", len(results))
	}
	if results[0].total != 0 {
		t.Errorf("total = %d, want 0 (the file is all comments)", results[0].total)
	}
	if results[0].matchedNothing() {
		t.Error("a category whose globs matched a file is alive even when it tallies 0")
	}
}

// The dead-category check must run even when the category is within budget —
// otherwise the `0 <= budget` path returns `ok` first and the stale glob is never
// reported, which is the original defect exactly.
func TestDeadCategoryIsReportedEvenThoughZeroIsWithinBudget(t *testing.T) {
	root := writeBudgetRepo(t,
		"categories:\n  core:\n    kind: go-logic\n    budget: 500\n    include:\n      - \"nowhere/*.go\"\n",
		map[string]string{"tools/cmd/llz/a.go": "package main\n"})

	var out, errOut bytes.Buffer
	err := runBudgetGateTo("core-surface", root, "b.yaml", false, coreSurfaceRemedy, &out, &errOut)
	if err == nil {
		t.Fatal("0 is within a budget of 500, but the category is dead and must fail")
	}
}

// `exact` makes a SHRINK a failure too. Without it the high-water number only
// bounds the measurement instead of equalling it, so a change that reduces the
// package and forgets to lower the line leaves slack the next change grows into —
// silently restoring the ceiling-with-headroom model, at the moment decomposition
// starts working.
func TestExactBudgetFailsWhenThePackageShrinks(t *testing.T) {
	files := map[string]string{"tools/cmd/llz/a.go": "package main\nvar x = 1\n"} // 2 logic lines
	mk := func(budget int, exact bool) string {
		cfg := "categories:\n  core:\n    kind: go-logic\n    budget: " + strconv.Itoa(budget) + "\n"
		if exact {
			cfg += "    exact: true\n"
		}
		return writeBudgetRepo(t, cfg+"    include:\n      - \"tools/cmd/llz/*.go\"\n", files)
	}
	var out, errOut bytes.Buffer
	err := runBudgetGateTo("core-surface", mk(50, true), "b.yaml", false, coreSurfaceRemedy, &out, &errOut)
	if err == nil {
		t.Fatal("an exact budget above the measurement must fail — that slack is undeclared")
	}
	// the operator must be told the number to write, not left to compute it
	if !strings.Contains(errOut.String(), "lower it to 2") {
		t.Errorf("the error must name the new number, got:\n%s", errOut.String())
	}
	if !strings.Contains(out.String(), "SHRANK") {
		t.Errorf("the report line must not read `ok`, got:\n%s", out.String())
	}
	// exactly equal passes
	out.Reset()
	errOut.Reset()
	if err := runBudgetGateTo("core-surface", mk(2, true), "b.yaml", false, coreSurfaceRemedy, &out, &errOut); err != nil {
		t.Errorf("an exact budget EQUAL to the measurement must pass: %v", err)
	}
	// and without exact, slack is the documented +3% convention untestable-loc uses
	out.Reset()
	errOut.Reset()
	if err := runBudgetGateTo("untestable-loc", mk(50, false), "b.yaml", false, untestableRemedy, &out, &errOut); err != nil {
		t.Errorf("without exact, headroom must remain legal: %v", err)
	}
}
