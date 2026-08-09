package sourceref

import (
	"strings"
	"testing"
)

// ── the symbol table ────────────────────────────────────────────────────────

// Missing a declaration KIND is a false positive, not a miss: an undeclared name
// makes every honest reference to it a finding. Each form is pinned separately.
func TestExportedDeclsCoversEveryDeclarationForm(t *testing.T) {
	root := writeTree(t, map[string]string{"tools/p/p.go": `package p

func Exported() {}
func unexported() {}
func (r Recv) Method() {}
func (r Recv) unexportedMethod() {}

type Typ struct{}
type unexportedTyp struct{}

var Single = 1
var Multi1, Multi2 = 1, 2

const (
	GroupedA = "a"
	GroupedB = "b"
)

var (
	BlockA = 1
	BlockB = 2
)
`})
	repo := repoFor(t, root)
	table, err := symbolTable(repo)
	if err != nil {
		t.Fatal(err)
	}
	syms := table["p"]
	for _, want := range []string{
		"Exported", "Method", "Typ", "Single",
		"Multi1", "Multi2", "GroupedA", "GroupedB", "BlockA", "BlockB",
	} {
		if !syms[want] {
			t.Errorf("%q should be in the table; a missing name reports honest references as stale", want)
		}
	}
	for _, notWant := range []string{"unexported", "unexportedMethod", "unexportedTyp"} {
		if syms[notWant] {
			t.Errorf("%q is unexported and cannot be referenced as pkg.%s", notWant, notWant)
		}
	}
}

// Methods count as part of their package's surface: this repo cites them as
// pkg.Method, naming the package a reader would go to rather than the receiver.
func TestMethodsArePartOfThePackageSurface(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/clusterspec/x.go": "package clusterspec\ntype LandingZone struct{}\nfunc (l *LandingZone) AplChartVersionWarnings() {}\n",
		"docs/a.md":              "see `clusterspec.AplChartVersionWarnings`.\n",
	})
	if err := RunSymbols(root); err != nil {
		t.Fatalf("a method reference must resolve: %v", err)
	}
}

// ── what is judged, and what is not ─────────────────────────────────────────

func TestRunSymbolsFailsOnASymbolThePackageDoesNotExport(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/tokeninv/x.go": "package tokeninv\nfunc Validate() {}\n",
		"docs/a.md":           "this file referenced `tokeninv.TokenValidity`.\n",
	})
	err := RunSymbols(root)
	if err == nil {
		t.Fatal("a symbol the package does not export must fail")
	}
	if !strings.Contains(err.Error(), "1 stale reference") {
		t.Errorf("the error should count findings, got %v", err)
	}
}

// THE CONVENTION. History is written possessively so it stays sayable without
// leaving a pointer that only looks live. Without this the guard is unshippable
// here — every finding on its first real run was a sentence of exactly this kind.
func TestPossessiveFormIsHistoryAndIsNotJudged(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/tokeninv/x.go": "package tokeninv\nfunc Validate() {}\n",
		// The live reference is what gives the run something to judge; without it the
		// Refs==0 arm fires and the possessive is never reached.
		"docs/a.md": "`tokeninv.Validate` runs it; it USED TO READ tokeninv's TokenValidity.\n",
	})
	if err := RunSymbols(root); err != nil {
		t.Fatalf("possessive form records history and must not be judged: %v", err)
	}
}

// A package we cannot see the source of gets no opinion: a finding would only
// ever mean "not vendored here".
func TestThirdPartyPackagesAreSkipped(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go": "package p\nfunc F() {}\n",
		"docs/a.md":    "`p.F` uses `cobra.Command`, `strings.Contains` and `yaml.Unmarshal`.\n",
	})
	if err := RunSymbols(root); err != nil {
		t.Fatalf("third-party references must be skipped, not reported: %v", err)
	}
}

// The reason Go input goes through the parser at all. `repo`, `registry` and
// `render` are real package names here, so a local variable of that name in a
// function body would read as a package reference to any text-level scan.
func TestGoCodeIsNotScannedOnlyComments(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/repo/r.go": "package repo\nfunc Open() {}\n",
		// Gives the run something to judge, so the Refs==0 arm does not fire first.
		"docs/a.md": "`repo.Open` is the entry point.\n",
		"tools/user/u.go": `package user

func f() {
	repo := struct{ ReadFile func() }{}
	_ = repo.ReadFile
}
`,
	})
	if err := RunSymbols(root); err != nil {
		t.Fatalf("a local variable sharing a package name is code, not a reference: %v", err)
	}
}

func TestCommentsInGoFilesAreScanned(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/repo/r.go": "package repo\nfunc Open() {}\n",
		"tools/user/u.go": "package user\n\n// This reaches for repo.Closed, which does not exist.\nfunc f() {}\n",
	})
	err := RunSymbols(root)
	if err == nil {
		t.Fatal("a stale reference in a Go comment must fail — that is where this rot lives")
	}
	if !strings.Contains(err.Error(), "stale reference") {
		t.Errorf("unexpected error: %v", err)
	}
}

// An abbreviation standing for a family of symbols must match at least one of
// them — the same rule the path guard applies to a glob.
func TestWildcardReferenceMatchesAFamily(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/versionpins/v.go": "package versionpins\nconst (\n\tCITofuTag = \"1\"\n\tCIKubernetesTag = \"2\"\n)\n",
		"docs/a.md":              "two consts -> `versionpins.CI*Tag`\n",
	})
	if err := RunSymbols(root); err != nil {
		t.Fatalf("a wildcard matching real symbols must resolve: %v", err)
	}
}

func TestWildcardMatchingNothingFails(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/versionpins/v.go": "package versionpins\nconst Other = \"1\"\n",
		"docs/a.md":              "two consts -> `versionpins.CI*Tag`\n",
	})
	if err := RunSymbols(root); err == nil {
		t.Fatal("a wildcard whose whole family is gone must still fail")
	}
}

// ── fail-closed arms ────────────────────────────────────────────────────────

// The axis the path guard does not have. An empty table makes every reference
// look third-party and skips the lot — a green earned by indexing nothing.
func TestRunSymbolsFailsOnAnEmptySymbolTable(t *testing.T) {
	root := writeTree(t, map[string]string{"docs/a.md": "prose citing `foo.Bar`.\n"})
	err := RunSymbols(root)
	if err == nil {
		t.Fatal("indexing no packages must fail rather than skipping every reference")
	}
	if !strings.Contains(err.Error(), "empty table") {
		t.Errorf("the error should name the empty table, got %v", err)
	}
}

func TestRunSymbolsFailsWhenNoReferenceIsExtracted(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go": "package p\nfunc F() {}\n",
		"docs/a.md":    "prose that cites no package symbol at all\n",
	})
	err := RunSymbols(root)
	if err == nil {
		t.Fatal("a corpus yielding zero references means the extraction is broken")
	}
	if !strings.Contains(err.Error(), "not one") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Unparseable Go must not be a silent skip: its comments would go unchecked
// while the run still reported a coverage it did not have.
func TestUnparseableGoFailsRatherThanSkipping(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go":   "package p\nfunc F() {}\n",
		"tools/bad/b.go": "package bad\nfunc ( <<<not go\n",
	})
	if err := RunSymbols(root); err == nil {
		t.Fatal("unparseable Go must fail the run, not drop the file")
	}
}

// ── test-name citations ─────────────────────────────────────────────────────

// The class this half exists for. A citation names the mechanism that keeps a
// claim true — "TestX fails the build if they drift" — so when the test is
// renamed and the sentence is not, it still reads as a guarantee with nothing
// behind it. Two live instances sat in PRODUCTION source.
func TestRunSymbolsFailsOnACitedTestThatDoesNotExist(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go":      "package p\nfunc F() {}\n",
		"tools/p/p_test.go": "package p\nimport \"testing\"\nfunc TestGrantStatesTableIsPinned(t *testing.T) {}\n",
		"docs/a.md":         "`p.F` is pinned; notice TestGrantStatesIsPinned.\n",
	})
	err := RunSymbols(root)
	if err == nil {
		t.Fatal("a citation to a test that does not exist must fail")
	}
	if !strings.Contains(err.Error(), "1 stale reference") {
		t.Errorf("expected one finding, got %v", err)
	}
}

func TestRunSymbolsPassesACitedTestThatExists(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go":      "package p\nfunc F() {}\n",
		"tools/p/p_test.go": "package p\nimport \"testing\"\nfunc TestGrantStatesTableIsPinned(t *testing.T) {}\n",
		"docs/a.md":         "`p.F` is pinned; notice TestGrantStatesTableIsPinned.\n",
	})
	if err := RunSymbols(root); err != nil {
		t.Fatalf("a citation to a real test must resolve: %v", err)
	}
}

// Test files are READ for their function names and never SCANNED for citations:
// a test's fixtures invent names freely, so its text is not a claim about the
// repo while its function names are what everything else points at.
func TestTestFilesAreIndexedButNotScanned(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go": "package p\nfunc F() {}\n",
		// The fixture cites a test that does not exist. Scanning this file would
		// make the guard fail on its own test corpus.
		"tools/p/p_test.go": "package p\nimport \"testing\"\n// fixture cites TestNoSuchThingAnywhere\nfunc TestReal(t *testing.T) {}\n",
		"docs/a.md":         "`p.F`, covered by TestReal.\n",
	})
	if err := RunSymbols(root); err != nil {
		t.Fatalf("a citation inside a _test.go file must not be judged: %v", err)
	}
}

// A paragraph breaking a long test name across lines leaves a prefix, with or
// without a hyphen. Both live instances came from one config file. Reporting them
// would flag correct prose; the rule needs the line to END there AND a longer
// real test to exist, so a complete citation at a line break still resolves.
func TestCitationWrappedAcrossLinesIsSkipped(t *testing.T) {
	// Each body carries one COMPLETE citation as well, so the run has a test
	// reference to judge; without it the TestRefs==0 arm fires and the wrap is
	// never reached.
	for _, body := range []string{
		"a guard (TestSeedTargetsAreReserved-\n  Namespaces) covers it, as does\n" +
			"TestGlobalFlagsAreParsedBeforeRunE. `p.F`\n",
		"the comment explaining it. TestGlobalFlagsAreParsedBefore\n  RunE, alongside\n" +
			"TestSeedTargetsAreReservedNamespaces. `p.F`\n",
	} {
		root := writeTree(t, map[string]string{
			"tools/p/p.go": "package p\nfunc F() {}\n",
			"tools/p/p_test.go": "package p\nimport \"testing\"\n" +
				"func TestSeedTargetsAreReservedNamespaces(t *testing.T) {}\n" +
				"func TestGlobalFlagsAreParsedBeforeRunE(t *testing.T) {}\n",
			"docs/a.md": body,
		})
		if err := RunSymbols(root); err != nil {
			t.Errorf("a wrapped citation must be skipped, not reported: %v", err)
		}
	}
}

// The other half of the wrap rule: a citation that merely SHARES a prefix with a
// real test, mid-line, is stale and must still be caught.
func TestPrefixOfARealTestIsStillCaughtMidLine(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go":      "package p\nfunc F() {}\n",
		"tools/p/p_test.go": "package p\nimport \"testing\"\nfunc TestGuardExemptsItselfByDirectory(t *testing.T) {}\n",
		"docs/a.md":         "`p.F` — TestGuardExemptsItself fails if a real file stops matching.\n",
	})
	if err := RunSymbols(root); err == nil {
		t.Fatal("a stale prefix citation mid-line must be caught")
	}
}

// `Test` must be followed by an uppercase letter, which is what keeps ordinary
// words out of the extraction.
func TestOrdinaryWordsAreNotCitations(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go":      "package p\nfunc F() {}\n",
		"tools/p/p_test.go": "package p\nimport \"testing\"\nfunc TestReal(t *testing.T) {}\n",
		"docs/a.md":         "Testing this, we Tested it; the Tests pass. `p.F`, see TestReal.\n",
	})
	if err := RunSymbols(root); err != nil {
		t.Fatalf("Testing/Tested/Tests are not citations: %v", err)
	}
}

// Zero citations across a real corpus means testExpr broke, not that the repo
// stopped naming its tests.
func TestRunSymbolsFailsWhenNoTestCitationIsExtracted(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go":      "package p\nfunc F() {}\n",
		"tools/p/p_test.go": "package p\nimport \"testing\"\nfunc TestReal(t *testing.T) {}\n",
		"docs/a.md":         "`p.F` and nothing else.\n",
	})
	err := RunSymbols(root)
	if err == nil {
		t.Fatal("zero test citations must fail")
	}
	if !strings.Contains(err.Error(), "not one test citation") {
		t.Errorf("unexpected error: %v", err)
	}
}
