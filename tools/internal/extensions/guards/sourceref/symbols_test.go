package sourceref

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
	if err := RunSymbols(root, nil); err != nil {
		t.Fatalf("a method reference must resolve: %v", err)
	}
}

// ── what is judged, and what is not ─────────────────────────────────────────

func TestRunSymbolsFailsOnASymbolThePackageDoesNotExport(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/tokeninv/x.go": "package tokeninv\nfunc Validate() {}\n",
		"docs/a.md":           "this file referenced `tokeninv.TokenValidity`.\n",
	})
	err := RunSymbols(root, nil)
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
	if err := RunSymbols(root, nil); err != nil {
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
	if err := RunSymbols(root, nil); err != nil {
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
	if err := RunSymbols(root, nil); err != nil {
		t.Fatalf("a local variable sharing a package name is code, not a reference: %v", err)
	}
}

func TestCommentsInGoFilesAreScanned(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/repo/r.go": "package repo\nfunc Open() {}\n",
		"tools/user/u.go": "package user\n\n// This reaches for repo.Closed, which does not exist.\nfunc f() {}\n",
	})
	err := RunSymbols(root, nil)
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
	if err := RunSymbols(root, nil); err != nil {
		t.Fatalf("a wildcard matching real symbols must resolve: %v", err)
	}
}

func TestWildcardMatchingNothingFails(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/versionpins/v.go": "package versionpins\nconst Other = \"1\"\n",
		"docs/a.md":              "two consts -> `versionpins.CI*Tag`\n",
	})
	if err := RunSymbols(root, nil); err == nil {
		t.Fatal("a wildcard whose whole family is gone must still fail")
	}
}

// ── fail-closed arms ────────────────────────────────────────────────────────

// The axis the path guard does not have. An empty table makes every reference
// look third-party and skips the lot — a green earned by indexing nothing.
func TestRunSymbolsFailsOnAnEmptySymbolTable(t *testing.T) {
	root := writeTree(t, map[string]string{"docs/a.md": "prose citing `foo.Bar`.\n"})
	err := RunSymbols(root, nil)
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
	err := RunSymbols(root, nil)
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
	if err := RunSymbols(root, nil); err == nil {
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
	err := RunSymbols(root, nil)
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
	if err := RunSymbols(root, nil); err != nil {
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
	if err := RunSymbols(root, nil); err != nil {
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
		if err := RunSymbols(root, nil); err != nil {
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
	if err := RunSymbols(root, nil); err == nil {
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
	if err := RunSymbols(root, nil); err != nil {
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
	err := RunSymbols(root, nil)
	if err == nil {
		t.Fatal("zero test citations must fail")
	}
	if !strings.Contains(err.Error(), "not one test citation") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ── make-target citations ───────────────────────────────────────────────────

// `make lint` is cited 33 times and `make coverage` 10; the runbooks and skills
// tell a reader — often an agent — to run one. A renamed target turns every
// citation into a step that fails at the point of use.
func TestRunSymbolsFailsOnACitedMakeTargetThatDoesNotExist(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Makefile":     "lint:\n\techo hi\n",
		"tools/p/p.go": "package p\nfunc F() {}\n",
		"docs/a.md":    "run `make lint`, then `make coverage`. `p.F`\n",
	})
	err := RunSymbols(root, nil)
	if err == nil {
		t.Fatal("a citation to a target the Makefile does not declare must fail")
	}
	if !strings.Contains(err.Error(), "1 stale reference") {
		t.Errorf("expected one finding, got %v", err)
	}
}

func TestRunSymbolsPassesCitedMakeTargetsThatExist(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Makefile":     "lint:\n\techo hi\ncoverage:\n\techo hi\n",
		"tools/p/p.go": "package p\nfunc F() {}\n",
		"docs/a.md":    "run `make lint`, then `make coverage`. `p.F`\n",
	})
	if err := RunSymbols(root, nil); err != nil {
		t.Fatalf("both targets exist: %v", err)
	}
}

// `make` is an ordinary English verb. An unanchored scan finds "make sure" and
// "make it impossible", which swamps the signal — measured before choosing the
// backticked form.
func TestBareMakeInProseIsNotACitation(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Makefile":     "lint:\n\techo hi\n",
		"tools/p/p.go": "package p\nfunc F() {}\n",
		"docs/a.md":    "make sure to make everything work; that would make sense. `make lint`, `p.F`\n",
	})
	if err := RunSymbols(root, nil); err != nil {
		t.Fatalf("prose uses of the verb are not citations: %v", err)
	}
}

// A rule is declared at column zero; a TAB-indented recipe line can contain a
// colon and is not a target. Indexing one would let a citation resolve against
// something that is not a rule.
func TestRecipeLinesAreNotIndexedAsTargets(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Makefile":     "lint:\n\t@echo running: coverage\n",
		"tools/p/p.go": "package p\nfunc F() {}\n",
		"docs/a.md":    "run `make coverage`. `p.F`\n",
	})
	if err := RunSymbols(root, nil); err == nil {
		t.Fatal("`coverage` appears only inside a recipe, so the citation must fail")
	}
}

// Target-specific variables (`t: export VAR := x`) still declare the target.
func TestTargetSpecificVariableLineStillDeclaresTheTarget(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Makefile":     "docs-guard: export LLZ_FORCE_SOURCE := 1\ndocs-guard:\n\techo hi\n",
		"tools/p/p.go": "package p\nfunc F() {}\n",
		"docs/a.md":    "run `make docs-guard`. `p.F`\n",
	})
	if err := RunSymbols(root, nil); err != nil {
		t.Fatalf("a target-specific variable line declares the target: %v", err)
	}
}

// ── llz ci verb citations ───────────────────────────────────────────────────

// treeWithCIVerbs builds a cobra root carrying an `llz ci` group, which is what
// the verb half indexes. The list comes from the LIVE tree in production, never a
// literal — see civerbs.go — so a fixture is the only place one is written out.
func treeWithCIVerbs(verbs ...string) *cobra.Command {
	root := &cobra.Command{Use: "llz"}
	ci := &cobra.Command{Use: "ci"}
	for _, v := range verbs {
		ci.AddCommand(&cobra.Command{Use: v})
	}
	root.AddCommand(ci)
	return root
}

// The class this half exists for: a runbook step that answers `unknown command`
// at the moment someone is following it.
func TestRunSymbolsFailsOnACitedCIVerbThatDoesNotExist(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go": "package p\nfunc F() {}\n",
		"docs/a.md":    "run `llz ci drain-buckets`. `p.F`\n",
	})
	err := RunSymbols(root, treeWithCIVerbs("drain-obj-buckets"))
	if err == nil {
		t.Fatal("a citation to a verb the tree does not carry must fail")
	}
	if !strings.Contains(err.Error(), "1 stale reference") {
		t.Errorf("expected one finding, got %v", err)
	}
}

func TestRunSymbolsPassesACitedCIVerbThatExists(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go": "package p\nfunc F() {}\n",
		"docs/a.md":    "run `llz ci drain-obj-buckets`. `p.F`\n",
	})
	if err := RunSymbols(root, treeWithCIVerbs("drain-obj-buckets")); err != nil {
		t.Fatalf("a real verb must resolve: %v", err)
	}
}

// `llz ci assert-*` names a family, exactly as `versionpins.CI*Tag` does for
// symbols. It must match at least one member — a family whose members have ALL
// been renamed is as stale as a single verb that has.
func TestCIVerbFamilyGlobMatchesAtLeastOneMember(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go": "package p\nfunc F() {}\n",
		"docs/a.md":    "the `llz ci assert-*` family. `p.F`\n",
	})
	if err := RunSymbols(root, treeWithCIVerbs("assert-network", "assert-secrets")); err != nil {
		t.Fatalf("a family with members must resolve: %v", err)
	}
	if err := RunSymbols(root, treeWithCIVerbs("converge")); err == nil {
		t.Fatal("a family with no members left must fail")
	}
}

// THE CONVENTION. A retired verb is named bare so it stops reading as something
// to run; seven of the first run's findings were correct sentences of this shape.
func TestBareVerbNameIsHistoryAndIsNotJudged(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go": "package p\nfunc F() {}\n",
		"docs/a.md":    "the retired `gen-bootstrap-tls` step; run `llz ci converge`. `p.F`\n",
	})
	if err := RunSymbols(root, treeWithCIVerbs("converge")); err != nil {
		t.Fatalf("a bare verb name records history and must not be judged: %v", err)
	}
}

// A tree with no `ci` group cannot answer the question. Judging anyway would
// report every citation in the repo — the same call the missing-Makefile arm makes.
func TestNoCIGroupJudgesNothing(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go": "package p\nfunc F() {}\n",
		"docs/a.md":    "run `llz ci anything-at-all`. `p.F`\n",
	})
	if err := RunSymbols(root, &cobra.Command{Use: "llz"}); err != nil {
		t.Fatalf("no ci group means nothing to judge against: %v", err)
	}
	if err := RunSymbols(root, nil); err != nil {
		t.Fatalf("a nil tree disables the half rather than failing it: %v", err)
	}
}

// ── ADR citations and the index ─────────────────────────────────────────────

// adrTree writes an ADR directory plus an index, so both halves have something
// to disagree about.
func adrTree(t *testing.T, index string, files ...string) string {
	t.Helper()
	m := map[string]string{
		"tools/p/p.go":       "package p\nfunc F() {}\n",
		"docs/adr/README.md": index,
		// Carries one citation so the ADRRefs==0 arm does not fire before the case
		// under test is reached.
		"docs/other.md": "see ADR 0001. `p.F`\n",
	}
	for _, f := range files {
		m["docs/adr/"+f] = "# " + f + "\n"
	}
	return writeTree(t, m)
}

// The defect that motivated the index half: a row stating an absence that is not
// true. It reads as deliberate, which is what makes it worse than a missing row.
func TestADRIndexCatchesAReservedRowWhoseFileExists(t *testing.T) {
	root := adrTree(t, "| 0001 | *Reserved* — not written |\n", "0001-pat-rotation-locus.md")
	err := RunSymbols(root, nil)
	if err == nil {
		t.Fatal("a reserved row beside a real file must fail")
	}
	if !strings.Contains(err.Error(), "1 ADR index disagreement") {
		t.Errorf("expected an index disagreement, got %v", err)
	}
}

func TestADRIndexCatchesAFileWithNoRow(t *testing.T) {
	root := adrTree(t, "| [0001](0001-a.md) | A |\n", "0001-a.md", "0002-b.md")
	if err := RunSymbols(root, nil); err == nil {
		t.Fatal("an ADR the index never lists must fail")
	}
}

func TestADRIndexCatchesARowLinkingAMissingFile(t *testing.T) {
	root := adrTree(t, "| [0001](0001-a.md) | A |\n| [0002](0002-gone.md) | B |\n", "0001-a.md")
	if err := RunSymbols(root, nil); err == nil {
		t.Fatal("a row linking a file that is not there must fail")
	}
}

// A genuine reservation — a row with no link and no file — is the whole point of
// the index carrying unwritten numbers, and must pass.
func TestADRIndexAcceptsAGenuineReservation(t *testing.T) {
	root := adrTree(t, "| [0001](0001-a.md) | A |\n| 0011 | *Reserved* — not written |\n", "0001-a.md")
	if err := RunSymbols(root, nil); err != nil {
		t.Fatalf("a reservation with no file is correct: %v", err)
	}
}

// A citation of an ambiguous number must disambiguate. The rule keys on "more
// than one file carries this number", so it stops applying if the duplicate is
// ever resolved and starts if a second one appears.
func TestBareCitationOfADuplicatedADRNumberFails(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go":       "package p\nfunc F() {}\n",
		"docs/adr/README.md": "| [0007](0007-a.md) | A |\n| [0007](0007-b.md) | B |\n",
		"docs/adr/0007-a.md": "# a\n",
		"docs/adr/0007-b.md": "# b\n",
		"docs/use.md":        "governed by ADR 0007 and nothing else. `p.F`\n",
	})
	err := RunSymbols(root, nil)
	if err == nil {
		t.Fatal("a bare citation of a duplicated number is ambiguous and must fail")
	}
	if !strings.Contains(err.Error(), "stale reference") {
		t.Errorf("unexpected error: %v", err)
	}
}

// An ADR titling itself with its own number is not a citation needing
// disambiguation — it is the document being disambiguated. Skipping it is the
// same call docs-guard makes for the doc that teaches its own mechanism.
func TestAnADRsOwnTitleIsNotJudged(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go":       "package p\nfunc F() {}\n",
		"docs/adr/README.md": "| [0007](0007-a.md) | A |\n| [0007](0007-b.md) | B |\n",
		"docs/adr/0007-a.md": "# ADR 0007 — the first one\n",
		"docs/adr/0007-b.md": "# ADR 0007 — the second one\n",
		"docs/use.md":        "see ADR 0007 (the first one). `p.F`\n",
	})
	if err := RunSymbols(root, nil); err != nil {
		t.Fatalf("an ADR naming its own number must not be judged: %v", err)
	}
}

func TestQualifiedCitationOfADuplicatedNumberPasses(t *testing.T) {
	idx := "| [0007](0007-a.md) | A |\n| [0007](0007-b.md) | B |\n"
	root := writeTree(t, map[string]string{
		"tools/p/p.go":       "package p\nfunc F() {}\n",
		"docs/adr/README.md": idx,
		"docs/adr/0007-a.md": "# a\n",
		"docs/adr/0007-b.md": "# b\n",
		"docs/use.md":        "see ADR 0007 (state encryption). `p.F`\n",
	})
	if err := RunSymbols(root, nil); err != nil {
		t.Fatalf("a qualified citation is exactly what the convention asks for: %v", err)
	}
}

// apl-core numbers its ADRs by date. Four digits, not ours — the leading zero is
// what separates them.
func TestUpstreamDateNumberedADRsAreNotJudged(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/p/p.go":       "package p\nfunc F() {}\n",
		"docs/adr/README.md": "| [0001](0001-a.md) | A |\n",
		"docs/adr/0001-a.md": "# a\n",
		"docs/use.md":        "upstream ADR 2026-06-02-release-branch-per-cycle. See ADR 0001. `p.F`\n",
	})
	if err := RunSymbols(root, nil); err != nil {
		t.Fatalf("an upstream date-numbered ADR is not ours to judge: %v", err)
	}
}
