package budget

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCountGoLogicLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"blank lines only", "\n\n   \n\t\n", 0},
		{"line comments only", "// a\n\t// indented\n//\n", 0},
		{"code only", "package main\nfunc f() {}\n", 2},
		{"trailing comment counts as code", "x := 1 // note\n", 1},
		{"mixed", "// doc\npackage main\n\nfunc f() {\n\treturn\n}\n", 4},
		{
			// The reason this counter does NOT track /* … */: `/*` in this repo
			// lives inside glob and regex literals. A block-comment scanner would
			// treat the first as an unterminated comment and swallow everything
			// after it. All four of these lines are code.
			name: "glob literal is not a block comment",
			in: "a := \"tools/cmd/llz/*.go\"\n" +
				"b := \"terraform-iac-bootstrap/*/.terraform.lock.hcl\"\n" +
				"c := regexp.MustCompile(`/\\*`)\n" +
				"d := 1\n",
			want: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countGoLogicLines(tt.in); got != tt.want {
				t.Errorf("countGoLogicLines() = %d, want %d", got, tt.want)
			}
		})
	}
}

// countScriptLines and countGoLogicLines share a body; this pins the one thing
// that differs, so a refactor cannot silently give Go files the `#` rule.
func TestCountersUseTheirOwnCommentMarker(t *testing.T) {
	const src = "// go comment\n# shell comment\n"
	if got := countGoLogicLines(src); got != 1 {
		t.Errorf("countGoLogicLines: the `#` line is Go code, want 1, got %d", got)
	}
	if got := countScriptLines(src); got != 1 {
		t.Errorf("countScriptLines: the `//` line is shell code, want 1, got %d", got)
	}
}

// writeCoreSurfaceRepo is writeBudgetRepo with the shipped core-surface shape:
// a go-logic category over tools/cmd/llz, excluding tests. The config name differs
// from b.yaml because these cases exercise the command's default --config.
func writeCoreSurfaceRepo(t *testing.T, budget int, files map[string]string) string {
	t.Helper()
	root := writeBudgetRepo(t, "", files)
	cfg := "categories:\n" +
		"  core:\n" +
		"    kind: go-logic\n" +
		"    budget: " + strconv.Itoa(budget) + "\n" +
		"    include:\n" +
		"      - \"tools/cmd/llz/*.go\"\n" +
		"exclude:\n" +
		"  - \"tools/cmd/llz/*_test.go\"\n"
	if err := os.WriteFile(filepath.Join(root, ".core-surface-budget.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCoreSurfaceExcludesTestsAndInternal(t *testing.T) {
	root := writeCoreSurfaceRepo(t, 100, map[string]string{
		"tools/cmd/llz/a.go":         "package main\nfunc A() {}\n",         // 2
		"tools/cmd/llz/a_test.go":    "package main\nfunc TestA() {}\nx\n",  // excluded
		"tools/internal/pkg/b.go":    "package pkg\nfunc B() {}\nvar y=1\n", // not included
		"tools/cmd/llz/sub/deep.go":  "package sub\nfunc C() {}\n",          // *.go is one level only
		"tools/cmd/llz/.core-notes":  "ignored\n",
		".core-surface-budget-x.yml": "ignored\n",
	})
	cfg, err := loadBudgetConfig(filepath.Join(root, ".core-surface-budget.yaml"))
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
	if results[0].total != 2 {
		t.Errorf("total = %d, want 2 (only tools/cmd/llz/a.go); files=%v", results[0].total, results[0].files)
	}
	for _, f := range results[0].files {
		if strings.Contains(f.path, "_test.go") || strings.Contains(f.path, "internal") || strings.Contains(f.path, "/sub/") {
			t.Errorf("counted a file it must not: %s", f.path)
		}
	}
}

func TestCoreSurfaceFailsOverBudgetAndPassesUnder(t *testing.T) {
	files := map[string]string{
		"tools/cmd/llz/a.go": "package main\nfunc A() {}\nvar x = 1\n", // 3 logic lines
	}
	if err := Run("core-surface", writeCoreSurfaceRepo(t, 3, files), ".core-surface-budget.yaml", false, CoreSurfaceRemedy); err != nil {
		t.Errorf("at exactly budget the gate must pass, got %v", err)
	}
	err := Run("core-surface", writeCoreSurfaceRepo(t, 2, files), ".core-surface-budget.yaml", false, CoreSurfaceRemedy)
	if err == nil {
		t.Fatal("over budget must fail")
	}
	if !strings.Contains(err.Error(), "core-surface gate failed") {
		t.Errorf("error should name the core-surface gate, got %v", err)
	}
}

// The remedy is the reason this gate is not just a category in
// .untestable-budget.yaml: the two gates push in opposite directions, so a
// breach here must not tell the reader to move logic INTO tools/cmd/llz.
func TestCoreSurfaceRemedyPointsOutOfPackageMain(t *testing.T) {
	if strings.Contains(UntestableRemedy, "internal") {
		t.Error("the untestable-loc remedy should point INTO tools/cmd/llz")
	}
	if !strings.Contains(UntestableRemedy, "tools/cmd/llz") {
		t.Error("the untestable-loc remedy names tools/cmd/llz as the destination")
	}
	for _, want := range []string{"tools/internal", "extension", "{config}"} {
		if !strings.Contains(CoreSurfaceRemedy, want) {
			t.Errorf("core-surface remedy should mention %q; got %q", want, CoreSurfaceRemedy)
		}
	}
}

// A config-supplied remedy must win over the gate's default, since that is the
// only thing distinguishing the two gates' output.
func TestBudgetGateUsesConfigRemedy(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tools/cmd/llz"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tools/cmd/llz/a.go"), []byte("package main\nvar x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := "remedy: MOVE IT OUT\ncategories:\n  core:\n    kind: go-logic\n    budget: 0\n    include:\n      - \"tools/cmd/llz/*.go\"\n"
	if err := os.WriteFile(filepath.Join(root, "b.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := loadBudgetConfig(filepath.Join(root, "b.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Remedy != "MOVE IT OUT" {
		t.Fatalf("remedy did not parse: %q", parsed.Remedy)
	}
	if err := Run("core-surface", root, "b.yaml", false, CoreSurfaceRemedy); err == nil {
		t.Error("budget 0 must fail")
	}
}

// An unknown kind must be a hard error, not a silently-zero category — a typo'd
// kind would otherwise report a passing 0 forever.
func TestUnknownKindIsAnError(t *testing.T) {
	root := t.TempDir()
	cfg := "categories:\n  core:\n    kind: go-logick\n    budget: 1\n    include:\n      - \"**/*.go\"\n"
	if err := os.WriteFile(filepath.Join(root, "b.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := loadBudgetConfig(filepath.Join(root, "b.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scanBudgetCategories(root, parsed); err == nil {
		t.Error("unknown kind must fail the scan")
	} else if !strings.Contains(err.Error(), "go-logic") {
		t.Errorf("the error should list go-logic among valid kinds, got %v", err)
	}
}

// ── output is the product (ADR 0014) ─────────────────────────────────────────
//
// Everything these gates exist to do happens on stdout/stderr: naming the
// offending category, printing the remedy that makes the ratchet teach rather
// than merely block, and emitting an annotation GitHub renders on the diff. All
// of it used to be unassertable, and two mutants survived the whole suite —
// deleting the {config} substitution, and breaking the ::error:: prefix. These
// pin both.

// budgetFixture is writeBudgetRepo with a single 3-line Go file, for the cases
// that only need "a category that breaches its budget".
func budgetFixture(t *testing.T, budget int, remedy string) string {
	t.Helper()
	cfg := ""
	if remedy != "" {
		cfg += "remedy: " + remedy + "\n"
	}
	cfg += "categories:\n  core:\n    kind: go-logic\n    budget: " + strconv.Itoa(budget) +
		"\n    include:\n      - \"tools/cmd/llz/*.go\"\n"
	return writeBudgetRepo(t, cfg, map[string]string{
		"tools/cmd/llz/a.go": "package main\nvar x = 1\nvar y = 2\n"})
}

// GitHub only parses an annotation command at the START of a line. If the prefix
// breaks, the build still exits 1 but the annotation silently stops rendering —
// the failure becomes invisible in the PR while CI stays red, which is the worst
// of both. ci_guards.go documents this hazard; this asserts it.
func TestBudgetGateEmitsAGitHubAnnotationAtLineStart(t *testing.T) {
	var out, errOut bytes.Buffer
	err := RunTo("core-surface", budgetFixture(t, 1, ""), "b.yaml", false,
		CoreSurfaceRemedy, &out, &errOut)
	if err == nil {
		t.Fatal("over budget must fail")
	}
	var found bool
	for _, l := range strings.Split(errOut.String(), "\n") {
		if strings.HasPrefix(l, "::error::") {
			found = true
		}
	}
	if !found {
		t.Errorf("no line STARTS with ::error:: — GitHub will not render this:\n%s", errOut.String())
	}
}

// The remedy is the gate's whole pedagogical payload, and {config} is what points
// the reader at the file to edit. Dropping the substitution ships a literal
// "{config}" to operators.
func TestBudgetGateSubstitutesTheConfigPlaceholder(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := RunTo("core-surface", budgetFixture(t, 1, ""), "b.yaml", false,
		CoreSurfaceRemedy, &out, &errOut); err == nil {
		t.Fatal("over budget must fail")
	}
	got := errOut.String()
	if strings.Contains(got, "{config}") {
		t.Errorf("the {config} placeholder reached the operator unsubstituted:\n%s", got)
	}
	if !strings.Contains(got, "b.yaml") {
		t.Errorf("the remedy must name the config file to edit, got:\n%s", got)
	}
}

// A config-supplied remedy must win over the gate default — it is the only thing
// distinguishing two gates that otherwise print identically.
func TestBudgetGateConfigRemedyWinsAndIsPrinted(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := RunTo("core-surface", budgetFixture(t, 1, "MOVE-IT-OUT-{config}"), "b.yaml",
		false, CoreSurfaceRemedy, &out, &errOut); err == nil {
		t.Fatal("over budget must fail")
	}
	if !strings.Contains(errOut.String(), "MOVE-IT-OUT-b.yaml") {
		t.Errorf("the config remedy must be used and substituted, got:\n%s", errOut.String())
	}
}

// ── a gate that cannot fail ──────────────────────────────────────────────────

func TestCategoryMatchingNoFilesIsAHardFailure(t *testing.T) {
	root := budgetFixture(t, 99, "")
	cfg := "categories:\n  core:\n    kind: go-logic\n    budget: 99\n    include:\n      - \"tools/cmd/IIz/*.go\"\n"
	if err := os.WriteFile(filepath.Join(root, "b.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := RunTo("core-surface", root, "b.yaml", false, CoreSurfaceRemedy, &out, &errOut)
	if err == nil {
		t.Fatal("a category whose globs match nothing must fail — it can never fail otherwise")
	}
	if !strings.Contains(errOut.String(), "matched NO files") {
		t.Errorf("the error must say the globs matched nothing, got:\n%s", errOut.String())
	}
	if !strings.Contains(out.String(), "MATCHED NOTHING") {
		t.Errorf("the report line must not read `ok`, got:\n%s", out.String())
	}
}

func TestAllowEmptyOptsOutDeliberately(t *testing.T) {
	root := budgetFixture(t, 99, "")
	cfg := "categories:\n  core:\n    kind: go-logic\n    budget: 0\n    allowEmpty: true\n" +
		"    include:\n      - \"tools/cmd/IIz/*.go\"\n"
	if err := os.WriteFile(filepath.Join(root, "b.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := RunTo("core-surface", root, "b.yaml", false, CoreSurfaceRemedy, &out, &errOut); err != nil {
		t.Errorf("allowEmpty must permit an empty category: %v", err)
	}
}

// The distinction the fix turns on: a category that MATCHES files which happen to
// tally zero is alive (terraform-provisioner-bash is exactly this in the real
// config) and must not be confused with one whose globs match nothing.
func TestZeroTallyWithMatchedFilesIsNotDead(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tools/cmd/llz"), 0o755); err != nil {
		t.Fatal(err)
	}
	// matches the glob, but every line is a comment => tally 0
	if err := os.WriteFile(filepath.Join(root, "tools/cmd/llz/a.go"), []byte("// only\n// comments\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := "categories:\n  core:\n    kind: go-logic\n    budget: 0\n    include:\n      - \"tools/cmd/llz/*.go\"\n"
	if err := os.WriteFile(filepath.Join(root, "b.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := RunTo("core-surface", root, "b.yaml", false, CoreSurfaceRemedy, &out, &errOut); err != nil {
		t.Errorf("a matched-but-zero category is alive and within budget: %v\n%s", err, errOut.String())
	}
	if strings.Contains(out.String(), "MATCHED NOTHING") {
		t.Errorf("matched-but-zero must not be reported as dead:\n%s", out.String())
	}
}
