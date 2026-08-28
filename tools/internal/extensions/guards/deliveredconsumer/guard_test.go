package deliveredconsumer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── the parser ────────────────────────────────────────────────────────────────

func TestManagedEntriesReadsOnlyTheManagedClass(t *testing.T) {
	const manifest = `# a comment
managed  README.md
owned    kubernetes-custom/**
merge    .github/workflows/terraform.yml

managed  docs/**
# managed  commented-out.md
managed
`
	got := ManagedEntries(manifest)
	want := []string{"README.md", "docs/**"}
	if len(got) != len(want) {
		t.Fatalf("ManagedEntries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// ── the declaration corpus, which is the whole guard ──────────────────────────

// THE BUG THE FIRST CUT HAD, pinned so it cannot come back. Scanning every
// identifier made a retired symbol look present, because this repo leaves a
// tombstone comment explaining what was removed — which is the convention, and a
// good one. Reconstructing the real case against that corpus passed. Only
// DECLARATIONS can answer "does this still exist".
func TestATombstoneCommentIsNotADeclaration(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, "values.go", `package clusterspec

// values.go — the RenderValues pipeline that used to live here was RETIRED when
// LLZ moved to the managed App Platform. See ADR 0005.

func ValuesIdentity() {}
`)
	syms := corpusOf(t, root)
	if syms["RenderValues"] {
		t.Error("a symbol named only in a comment was counted as declared — the retired-consumer " +
			"case walks straight through a guard that believes this")
	}
	if !syms["ValuesIdentity"] {
		t.Error("a declared func was not found; the corpus is not reading declarations at all")
	}
}

func TestTheCorpusFindsEveryDeclarationForm(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, "decls.go", `package p

type Renderer struct{}

func TopLevelFunc() {}

func (r Renderer) MethodName() {}

const SingleConst = "x"

var SingleVar = 1

const (
	GroupedConst        = "y"
	GroupedTyped string = "z"
)

var (
	GroupedVar = 2
)
`)
	syms := corpusOf(t, root)
	for _, want := range []string{
		"Renderer", "TopLevelFunc", "MethodName",
		"SingleConst", "SingleVar", "GroupedConst", "GroupedTyped", "GroupedVar",
	} {
		if !syms[want] {
			t.Errorf("declaration %q was not found — rows naming it would fail for no reason", want)
		}
	}
}

// testdata must not vouch for a deleted symbol: fixtures deliberately carry
// retired names, and counting them would let a fixture keep a dead consumer alive.
func TestTestdataIsExcludedFromTheCorpus(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, filepath.Join("testdata", "fixture.go"), "package p\n\nfunc RetiredThing() {}\n")
	writeGo(t, root, "real.go", "package p\n\nfunc LiveThing() {}\n")
	syms := corpusOf(t, root)
	if syms["RetiredThing"] {
		t.Error("a declaration under testdata/ was counted — a fixture can now vouch for a deleted consumer")
	}
	if !syms["LiveThing"] {
		t.Error("the real declaration was missed; the walk is not reaching the tree")
	}
}

// ── the gate's fail-closed arms ───────────────────────────────────────────────

// AN UNREADABLE MANIFEST IS NOT AN EMPTY ONE. An empty manifest has zero managed
// entries, which every loop below reads as a clean pass — the "examined nothing"
// green this guard exists to prevent.
func TestAnAbsentManifestFails(t *testing.T) {
	err := Run(t.TempDir())
	if err == nil {
		t.Fatal("a root with no .template-manifest passed")
	}
	if !strings.Contains(err.Error(), ".template-manifest") {
		t.Errorf("the error does not name what it could not read: %v", err)
	}
}

// A MANIFEST WITH NO MANAGED ENTRIES IS ALSO A FAILURE. It parses, it is
// readable, and it means nothing is checked — which is exactly what a manifest
// whose format drifted looks like.
func TestAManifestWithNoManagedEntriesFails(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "owned    kubernetes-custom/**\n")
	writeGo(t, root, filepath.Join("tools", "x.go"), "package p\n\nfunc Anything() {}\n")
	err := Run(root)
	if err == nil {
		t.Fatal("a manifest declaring zero managed files passed — nothing was checked")
	}
	if !strings.Contains(err.Error(), "ZERO managed") {
		t.Errorf("the error does not say the corpus was empty: %v", err)
	}
}

// AN EMPTY GO CORPUS MUST NOT RENDER AS "every consumer is missing". That would
// bury the real cause (an unreadable tools/ tree) under a list of false findings.
func TestAnEmptyGoCorpusFailsWithoutBlamingTheRows(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "managed  README.md\n")
	err := Run(root)
	if err == nil {
		t.Fatal("a root with no Go source passed")
	}
	if strings.Contains(err.Error(), "no live consumer") {
		t.Errorf("an unreadable tools/ tree was reported as missing consumers: %v", err)
	}
	if !strings.Contains(err.Error(), "read no Go source") {
		t.Errorf("the error does not name the real cause: %v", err)
	}
}

// ── the registry itself ───────────────────────────────────────────────────────

// EVERY SHIPPED managed ENTRY HAS A ROW, checked against the REAL manifest rather
// than a fixture. This is the completeness half: adding a delivered file forces
// the author to answer "who reads this?", which is the question nobody asked
// about apl-values/values.yaml for a year.
func TestEveryShippedManagedEntryHasARow(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..",
		"instance-template", ".template-manifest"))
	if err != nil {
		t.Fatalf("read the real manifest: %v", err)
	}
	entries := ManagedEntries(string(raw))
	if len(entries) == 0 {
		t.Fatal("the real manifest parsed to zero managed entries — this test would pass vacuously")
	}
	for _, e := range entries {
		if _, ok := Consumers[e]; !ok {
			t.Errorf("`managed %s` is delivered and has no Consumers row", e)
		}
	}
}

// AND NO ROW OUTLIVES ITS ENTRY. A row for a file that is no longer delivered is
// a claim about nothing, and it makes the registry read as more complete than it is.
func TestNoRowOutlivesItsManifestEntry(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..",
		"instance-template", ".template-manifest"))
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{}
	for _, e := range ManagedEntries(string(raw)) {
		live[e] = true
	}
	for k := range Consumers {
		if !live[k] {
			t.Errorf("Consumers has a row for %q, which the manifest no longer classifies `managed` — "+
				"delete it, or the registry claims coverage it does not have", k)
		}
	}
}

// Every row carries a reason. A registry of names with no reasons is a list
// nobody can argue with, which is how a wrong row survives review.
func TestEveryRowCarriesAReason(t *testing.T) {
	for k, c := range Consumers {
		if strings.TrimSpace(c.Why) == "" {
			t.Errorf("%q has no reason", k)
		}
		if (c.Kind == ConsumerSymbol || c.Kind == ConsumerPath) && strings.TrimSpace(c.Ref) == "" {
			t.Errorf("%q is a checked kind with no Ref — it would be checked against nothing", k)
		}
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func writeGo(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, "tools", rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeManifest(t *testing.T, root, body string) {
	t.Helper()
	p := filepath.Join(root, "instance-template", ".template-manifest")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ── Run end to end, over a temp repo ──────────────────────────────────────────
//
// These drive the real Run against the real Consumers registry, so the arms below
// exercise the same lookups CI does. The manifest entries used are real rows —
// a fixture-only entry would test the guard against a registry it does not ship.

// THE PASS PATH. A human-consumed row needs no liveness check, so this is the
// shape where the guard must stay quiet.
func TestRunPassesWhenEveryEntryHasALiveConsumer(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "# comment\nmanaged  README.md\nowned    kubernetes-custom/**\n")
	writeGo(t, root, "x.go", "package p\n\nfunc Anything() {}\n")
	if err := Run(root); err != nil {
		t.Errorf("a manifest whose only managed entry is a human-consumed row failed: %v", err)
	}
}

// THE COMPLETENESS ARM: a delivered file nobody claimed.
func TestRunFailsOnAManagedEntryWithNoRow(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "managed  some/new/delivered-file.yaml\n")
	writeGo(t, root, "x.go", "package p\n\nfunc Anything() {}\n")
	err := Run(root)
	if err == nil {
		t.Fatal("an unclaimed delivered file passed — nobody had to say what reads it")
	}
	if !strings.Contains(err.Error(), "no live consumer") {
		t.Errorf("unexpected error: %v", err)
	}
}

// THE LIVENESS ARM for a symbol, driven through Run rather than the corpus
// helper: this is the apl-values/values.yaml case in the shape the gate runs it.
func TestRunFailsWhenANamedSymbolIsGone(t *testing.T) {
	root := t.TempDir()
	// `.template-removals` names ApplyTemplateRemovals as its consumer.
	writeManifest(t, root, "managed  .template-removals\n")
	writeGo(t, root, "x.go", "package sustain\n\n// ApplyTemplateRemovals was retired here.\nfunc Something() {}\n")
	err := Run(root)
	if err == nil {
		t.Fatal("a delivered file whose consumer no longer exists passed — this is the wedge")
	}
	if !strings.Contains(err.Error(), "no live consumer") {
		t.Errorf("unexpected error: %v", err)
	}
}

// …and the same entry passes once the symbol is declared, so the arm above is
// keyed to the symbol and not to the entry.
func TestRunPassesWhenTheNamedSymbolIsDeclared(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "managed  .template-removals\n")
	// The row names `sustain.ApplyTemplateRemovals`, so the package clause is part
	// of what has to match — that is the whole point of qualifying refs.
	writeGo(t, root, "x.go", "package sustain\n\nfunc ApplyTemplateRemovals() error { return nil }\n")
	if err := Run(root); err != nil {
		t.Errorf("a declared consumer was reported missing: %v", err)
	}
}

// THE LIVENESS ARM for a path.
func TestRunChecksPathConsumers(t *testing.T) {
	root := t.TempDir()
	// `.github/actions/**` names instance-template/.github/workflows as its consumer.
	writeManifest(t, root, "managed  .github/actions/**\n")
	writeGo(t, root, "x.go", "package p\n\nfunc Anything() {}\n")

	if err := Run(root); err == nil {
		t.Error("a path consumer that does not exist passed")
	}

	if err := os.MkdirAll(filepath.Join(root, "instance-template", ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Run(root); err != nil {
		t.Errorf("the path consumer exists now, but the guard still failed: %v", err)
	}
}

// THE COINCIDENCE THAT MADE A ROW PASS FOR THE WRONG REASON. An unqualified
// corpus of bare names let `.template-manifest`'s row, which said `Classify`, be
// satisfied by `baoread.Classify` — a function about OpenBao stderr. The symbol
// the row meant did not exist under that name anywhere, so the guard's one
// liveness arm was vouching for nothing on its own registry.
func TestAQualifiedRefIsNotSatisfiedByAnotherPackagesSymbol(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, filepath.Join("baoread", "read.go"), "package baoread\n\nfunc Classify() {}\n")
	syms := corpusOf(t, root)

	if !syms["baoread.Classify"] {
		t.Error("the qualified form was not recorded")
	}
	if syms["manifest.Classify"] {
		t.Error("a DIFFERENT package's Classify satisfied manifest.Classify — this is the " +
			"coincidence the qualified form exists to break")
	}
	// The bare form still resolves, deliberately: rows naming a package-unique
	// symbol should not fail on a technicality.
	if !syms["Classify"] {
		t.Error("the bare form stopped resolving — existing rows would fail for no reason")
	}
}

// Methods are recorded under their receiver's package too, so a row may name
// `pkg.Method` without knowing the receiver type.
func TestQualificationCoversMethodsAndGroupedDecls(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, filepath.Join("thing", "thing.go"), `package thing

type T struct{}

func (t T) Method() {}

const (
	Grouped = "x"
)
`)
	syms := corpusOf(t, root)
	for _, want := range []string{"thing.Method", "thing.Grouped", "thing.T"} {
		if !syms[want] {
			t.Errorf("%q was not recorded", want)
		}
	}
}

// GROUPED DECLARATIONS WITHOUT AN INITIALISER. The first cut of groupedDeclRe
// required `=`, so iota members past the first, `var ( Foo Bar )` and grouped
// `type ( Foo struct{} )` never entered the corpus — and a row naming one would
// have been told its consumer was retired. That is this guard reporting its own
// blind spot as someone else's bug.
func TestGroupedDeclarationsWithoutAnInitialiserAreFound(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, filepath.Join("kinds", "kinds.go"), `package kinds

type Kind int

const (
	FirstKind Kind = iota
	SecondKind
	ThirdKind
)

var (
	NoInitialiser Kind
	WithInit      = 3
)

type (
	GroupedType struct{}
)
`)
	syms := corpusOf(t, root)
	for _, want := range []string{
		"FirstKind", "SecondKind", "ThirdKind",
		"NoInitialiser", "WithInit", "GroupedType",
		"kinds.SecondKind", "kinds.GroupedType",
	} {
		if !syms[want] {
			t.Errorf("declaration %q was not found — a row naming it would get a false "+
				"'consumer retired'", want)
		}
	}
}

// STRUCT FIELDS AND INTERFACE METHODS ARE NOT DECLARATIONS. The regex this
// replaces admitted them — `Kind`, `Ref` and `Why` off this package's own
// Consumer struct all resolved — because a struct field sits at exactly the same
// indentation as a grouped const. A row naming one would resolve by coincidence,
// which is this guard's liveness arm vouching for nothing, for the second time.
func TestStructFieldsAndInterfaceMethodsAreNotDeclarations(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, filepath.Join("shapes", "shapes.go"), `package shapes

type Consumer struct {
	Kind ConsumerKind
	Ref  string
}

type Reader interface {
	ReadFile(p string) ([]byte, error)
}

const (
	RealGroupedConst = "x"
	BareIotaMember
)

var (
	NoInitialiser Consumer
)

type (
	GroupedType struct{}
)
`)
	syms := corpusOf(t, root)

	for _, notADecl := range []string{"Kind", "Ref", "ReadFile", "shapes.Kind", "shapes.ReadFile"} {
		if syms[notADecl] {
			t.Errorf("%q is a struct field or interface method, not a declaration — a row naming "+
				"it would resolve on a coincidence", notADecl)
		}
	}
	// …and every real grouped form still resolves, or the fix has traded one
	// false pass for a crop of false failures.
	for _, decl := range []string{
		"RealGroupedConst", "BareIotaMember", "NoInitialiser", "GroupedType",
		"shapes.BareIotaMember", "shapes.GroupedType", "shapes.Consumer", "shapes.Reader",
	} {
		if !syms[decl] {
			t.Errorf("declaration %q was not found — a row naming it gets a false 'consumer retired'", decl)
		}
	}
}
