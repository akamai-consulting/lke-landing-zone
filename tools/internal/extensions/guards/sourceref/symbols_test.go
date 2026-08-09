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
	if !strings.Contains(err.Error(), "1 stale symbol reference") {
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
	if !strings.Contains(err.Error(), "stale symbol reference") {
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
