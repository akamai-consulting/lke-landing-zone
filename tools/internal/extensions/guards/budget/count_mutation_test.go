package budget

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCategoryResultOverIsStrict pins the budget comparison as strictly greater.
// The budgets are set to the measured count of the day, so a category sitting
// exactly AT its budget is the normal steady state — reading that as over would
// red every PR the moment a ratchet lands.
func TestCategoryResultOverIsStrict(t *testing.T) {
	for _, tc := range []struct {
		total, budget int
		want          bool
	}{
		{4, 5, false},
		{5, 5, false}, // exactly at budget is within budget
		{6, 5, true},
		{0, 0, false},
		{1, 0, true},
	} {
		r := categoryResult{name: "c", total: tc.total, budget: tc.budget}
		if got := r.over(); got != tc.want {
			t.Errorf("categoryResult{total:%d,budget:%d}.over() = %v, want %v", tc.total, tc.budget, got, tc.want)
		}
	}
}

// TestScanUntestableOrderingAndBreakdown pins the two reporting properties the
// end-to-end tally test is blind to: categories come back in a stable
// alphabetical order (the gate's output is diffed across runs), and the
// per-file offender breakdown lists only files that actually contribute — a
// zero-count file in an over-budget report sends a reviewer to a file with
// nothing to convert.
func TestScanUntestableOrderingAndBreakdown(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("scripts/logic.sh", "set -e\nx=1\n")                     // 2
	write("scripts/header-only.sh", "#!/bin/bash\n# no logic\n\n") // 0 — must not appear in the breakdown
	write(".github/workflows/w.yml", "steps:\n  - run: |\n      echo a\n")

	cfg := budgetConfig{Categories: map[string]budgetCategory{
		"zulu-workflows": {Kind: "workflow-run", Budget: 9, Include: []string{".github/workflows/*.yml"}},
		"alpha-scripts":  {Kind: "script", Budget: 9, Include: []string{"scripts/**/*.sh"}},
	}}

	results, err := scanBudgetCategories(gRepo(root), cfg)
	if err != nil {
		t.Fatalf("scanBudgetCategories: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want 2 categories", results)
	}
	if results[0].name != "alpha-scripts" || results[1].name != "zulu-workflows" {
		t.Errorf("category order = %q, %q; want alphabetical", results[0].name, results[1].name)
	}
	sh := results[0]
	if sh.total != 2 {
		t.Errorf("alpha-scripts total = %d, want 2", sh.total)
	}
	if len(sh.files) != 1 || sh.files[0].path != "scripts/logic.sh" {
		t.Errorf("offender breakdown = %+v, want only scripts/logic.sh (the zero-count file is not an offender)", sh.files)
	}
}

// A single-line `run:` is tool-invocation glue and stays uncounted even when the
// step carries further, more-indented keys after it — those are YAML step
// configuration, not embedded shell. Counting them would penalise exactly the
// converted steps (`run: llz ci <verb>` + `env:`) this gate exists to reward.
func TestCountRunBlockLinesSingleLineRunWithNestedStepKeys(t *testing.T) {
	in := "" +
		"steps:\n" +
		"  - run: llz ci converge\n" +
		"    env:\n" +
		"      LOG_LEVEL: debug\n" +
		"      KUBECONFIG: /tmp/kubeconfig\n"
	if got := countRunBlockLines(in); got != 0 {
		t.Errorf("countRunBlockLines() = %d, want 0 (single-line run is glue; its step keys are not shell)", got)
	}
}

// A `command = <<EOT` heredoc whose terminator is missing (a truncated or
// malformed .tf) must be counted to the end of the file, not walked past it.
func TestCountTerraformProvisionerLinesUnterminatedHeredoc(t *testing.T) {
	in := "" +
		"  provisioner \"local-exec\" {\n" +
		"    command = <<-EOT\n" +
		"      kubectl apply -f x.yaml\n"
	if got := countTerraformProvisionerLines(in); got != 1 {
		t.Errorf("countTerraformProvisionerLines() = %d, want 1", got)
	}
}

func TestCountMakefileRecipeLinesEdges(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{
			// The line that ENDS a recipe run must be re-examined by the outer
			// loop: back-to-back rules are the common Makefile shape, and
			// swallowing the line after a recipe silently drops the next rule's
			// body from the tally — shell that moves into a Makefile then scores
			// as a reduction, the hole this category exists to close.
			name: "back-to-back rules are both counted",
			in: "" +
				"build:\n" +
				"\techo one\n" +
				"\techo two\n" +
				"publish:\n" +
				"\techo three\n" +
				"\techo four\n",
			want: 4,
		},
		{
			name: "a recipe followed immediately by a define macro counts both",
			in: "" +
				"build:\n" +
				"\techo one\n" +
				"\techo two\n" +
				"define STEP\n" +
				"echo x\n" +
				"echo y\n" +
				"endef\n",
			want: 4,
		},
		{
			// An unterminated `define` (no endef) must be counted to the end of
			// the file, not walked past it.
			name: "unterminated define stops at end of file",
			in: "" +
				"define STEP\n" +
				"echo a\n" +
				"echo b\n",
			want: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countMakefileRecipeLines(tt.in); got != tt.want {
				t.Errorf("countMakefileRecipeLines() = %d, want %d", got, tt.want)
			}
		})
	}
}
